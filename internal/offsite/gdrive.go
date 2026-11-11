package offsite

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/unmadethings/unthrecover/internal/connections"
)

// RootFolderName is the Drive folder unth creates for its copies.
const RootFolderName = "Unth Recover Backups"

const (
	folderMime = "application/vnd.google-apps.folder"
	chunkSize  = 8 << 20 // multiple of 256 KiB, as the resumable upload protocol requires
)

// GDrive stores copies in the user's own Google Drive, connected from the
// panel with Google's consent screen (scope drive.file: unth can only see the
// files it created).
type GDrive struct {
	g     *connections.Google
	store *connections.Store
	conn  *connections.Connection

	mu      sync.Mutex
	access  string
	expiry  time.Time
	folders map[string]string // "a/b" -> folder id
}

// NewGDrive loads a connection created in the panel.
func NewGDrive(g *connections.Google, store *connections.Store, name string) (*GDrive, error) {
	c, err := store.Load(name)
	if err != nil {
		return nil, err
	}
	if c.Provider != "gdrive" {
		return nil, fmt.Errorf("connection %s is not a Google Drive connection", name)
	}
	if !g.Configured() {
		return nil, errors.New("no Google OAuth client configured in this build (UNTH_GOOGLE_CLIENT_ID/UNTH_GOOGLE_CLIENT_SECRET)")
	}
	if c.ClientID != g.ClientID {
		return nil, fmt.Errorf("connection %s was created with another OAuth client; connect Google Drive again", name)
	}
	return &GDrive{g: g, store: store, conn: c, folders: map[string]string{}}, nil
}

// Target implements Backend.
func (d *GDrive) Target() string { return "gdrive:" + d.conn.Account }

func (d *GDrive) token(ctx context.Context, force bool) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !force && d.access != "" && time.Until(d.expiry) > time.Minute {
		return d.access, nil
	}
	tok, exp, err := d.g.Refresh(ctx, d.conn.RefreshToken)
	if err != nil {
		return "", fmt.Errorf("google drive access for %s: %w (if access was removed in the Google account, connect again in the panel)", d.conn.Account, err)
	}
	d.access, d.expiry = tok, exp
	return tok, nil
}

// do sends an authorized request, refreshing the token once on 401.
func (d *GDrive) do(ctx context.Context, method, u string, body func() io.Reader, hdr map[string]string) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		tok, err := d.token(ctx, attempt > 0)
		if err != nil {
			return nil, err
		}
		var b io.Reader
		if body != nil {
			b = body()
		}
		req, err := http.NewRequestWithContext(ctx, method, u, b)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := d.g.HTTP.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			_ = resp.Body.Close()
			continue
		}
		return resp, nil
	}
}

func apiError(resp *http.Response, what string) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := strings.TrimSpace(string(b))
	if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
		msg = e.Error.Message
	}
	return fmt.Errorf("google drive %s: HTTP %d: %s", what, resp.StatusCode, msg)
}

func (d *GDrive) getJSON(ctx context.Context, u, what string, out any) error {
	resp, err := d.do(ctx, http.MethodGet, u, nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return apiError(resp, what)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out)
}

type driveFile struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	MimeType string `json:"mimeType"`
}

func qEscape(s string) string { return strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) }

// children lists the (non-trashed) children of a folder, optionally by name.
func (d *GDrive) children(ctx context.Context, parent, name string) ([]driveFile, error) {
	q := fmt.Sprintf("'%s' in parents and trashed = false", qEscape(parent))
	if name != "" {
		q = fmt.Sprintf("'%s' in parents and name = '%s' and trashed = false", qEscape(parent), qEscape(name))
	}
	var all []driveFile
	pageToken := ""
	for {
		v := url.Values{"q": {q}, "fields": {"nextPageToken,files(id,name,mimeType)"}, "pageSize": {"1000"}, "spaces": {"drive"}}
		if pageToken != "" {
			v.Set("pageToken", pageToken)
		}
		var page struct {
			NextPageToken string      `json:"nextPageToken"`
			Files         []driveFile `json:"files"`
		}
		if err := d.getJSON(ctx, d.g.DriveURL+"/files?"+v.Encode(), "list", &page); err != nil {
			return nil, err
		}
		all = append(all, page.Files...)
		if page.NextPageToken == "" {
			return all, nil
		}
		pageToken = page.NextPageToken
	}
}

func (d *GDrive) createFolder(ctx context.Context, parent, name string) (string, error) {
	meta, _ := json.Marshal(map[string]any{"name": name, "mimeType": folderMime, "parents": []string{parent}})
	resp, err := d.do(ctx, http.MethodPost, d.g.DriveURL+"/files?fields=id", func() io.Reader { return bytes.NewReader(meta) },
		map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", apiError(resp, "create folder")
	}
	var f driveFile
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&f); err != nil || f.ID == "" {
		return "", errors.New("google drive create folder: no id")
	}
	return f.ID, nil
}

// Check makes sure the "Unth Recover Backups" folder exists (creating it on
// first use) and that the connection works.
func (d *GDrive) Check(ctx context.Context) error {
	if d.conn.FolderID != "" {
		var f driveFile
		err := d.getJSON(ctx, d.g.DriveURL+"/files/"+url.PathEscape(d.conn.FolderID)+"?fields=id,name,mimeType", "folder", &f)
		if err == nil && f.MimeType == folderMime {
			return nil
		}
		if err != nil && !strings.Contains(err.Error(), "HTTP 404") {
			return err
		}
	}
	// Reuse an existing "Unth Recover Backups" folder created by unth (e.g. the
	// old server's, on a new machine after a disaster) before creating one.
	var id string
	kids, err := d.children(ctx, "root", RootFolderName)
	if err != nil {
		return err
	}
	for _, k := range kids {
		if k.MimeType == folderMime {
			id = k.ID
			break
		}
	}
	if id == "" {
		if id, err = d.createFolder(ctx, "root", RootFolderName); err != nil {
			return err
		}
	}
	d.conn.FolderID = id
	d.mu.Lock()
	d.folders = map[string]string{}
	d.mu.Unlock()
	return d.store.Save(d.conn)
}

// dir resolves a slash path below the root folder; create makes missing folders.
func (d *GDrive) dir(ctx context.Context, p string, create bool) (string, error) {
	if d.conn.FolderID == "" {
		if err := d.Check(ctx); err != nil {
			return "", err
		}
	}
	p = strings.Trim(p, "/")
	if p == "" || p == "." {
		return d.conn.FolderID, nil
	}
	d.mu.Lock()
	id, ok := d.folders[p]
	d.mu.Unlock()
	if ok {
		return id, nil
	}
	parent, err := d.dir(ctx, path.Dir(p), create)
	if err != nil || parent == "" {
		return "", err
	}
	kids, err := d.children(ctx, parent, path.Base(p))
	if err != nil {
		return "", err
	}
	for _, k := range kids {
		if k.MimeType == folderMime {
			id = k.ID
			break
		}
	}
	if id == "" {
		if !create {
			return "", nil
		}
		if id, err = d.createFolder(ctx, parent, path.Base(p)); err != nil {
			return "", err
		}
	}
	d.mu.Lock()
	d.folders[p] = id
	d.mu.Unlock()
	return id, nil
}

func (d *GDrive) fileID(ctx context.Context, key string) (string, error) {
	parent, err := d.dir(ctx, path.Dir(key), false)
	if err != nil {
		return "", err
	}
	if parent == "" {
		return "", ErrNotExist
	}
	kids, err := d.children(ctx, parent, path.Base(key))
	if err != nil {
		return "", err
	}
	for _, k := range kids {
		if k.MimeType != folderMime {
			return k.ID, nil
		}
	}
	return "", ErrNotExist
}

// Put implements Backend with a resumable upload in 8 MiB chunks; an existing
// object of the same name is replaced after the new one is complete.
func (d *GDrive) Put(ctx context.Context, key string, r io.Reader, _ int64) error {
	parent, err := d.dir(ctx, path.Dir(key), true)
	if err != nil {
		return err
	}
	old, _ := d.children(ctx, parent, path.Base(key))
	meta, _ := json.Marshal(map[string]any{"name": path.Base(key), "parents": []string{parent}})
	resp, err := d.do(ctx, http.MethodPost, d.g.UploadURL+"/files?uploadType=resumable&fields=id",
		func() io.Reader { return bytes.NewReader(meta) },
		map[string]string{"Content-Type": "application/json; charset=UTF-8", "X-Upload-Content-Type": "application/octet-stream"})
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	session := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusOK || session == "" {
		return fmt.Errorf("google drive upload: HTTP %d starting the upload", resp.StatusCode)
	}
	br := bufio.NewReaderSize(r, chunkSize)
	buf := make([]byte, chunkSize)
	var off int64
	for {
		n, rerr := io.ReadFull(br, buf)
		if rerr != nil && rerr != io.ErrUnexpectedEOF && rerr != io.EOF {
			return rerr
		}
		last := rerr != nil
		if !last {
			if _, perr := br.Peek(1); perr == io.EOF {
				last = true
			} else if perr != nil {
				return perr
			}
		}
		var cr string
		switch {
		case last && n == 0:
			cr = fmt.Sprintf("bytes */%d", off)
		case last:
			cr = fmt.Sprintf("bytes %d-%d/%d", off, off+int64(n)-1, off+int64(n))
		default:
			cr = fmt.Sprintf("bytes %d-%d/*", off, off+int64(n)-1)
		}
		chunk := buf[:n]
		resp, err := d.do(ctx, http.MethodPut, session, func() io.Reader { return bytes.NewReader(chunk) },
			map[string]string{"Content-Range": cr})
		if err != nil {
			return err
		}
		if last {
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
				return apiError(resp, "upload")
			}
			var f driveFile
			if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&f); err != nil || f.ID == "" {
				return errors.New("google drive upload: no file id")
			}
			for _, o := range old { // replace semantics: remove previous objects of that name
				if o.ID != f.ID && o.MimeType != folderMime {
					_ = d.deleteID(ctx, o.ID)
				}
			}
			return nil
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 308 {
			return fmt.Errorf("google drive upload: HTTP %d for a chunk", resp.StatusCode)
		}
		off += int64(n)
	}
}

// Get implements Backend.
func (d *GDrive) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	id, err := d.fileID(ctx, key)
	if err != nil {
		return nil, err
	}
	resp, err := d.do(ctx, http.MethodGet, d.g.DriveURL+"/files/"+url.PathEscape(id)+"?alt=media", nil, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, ErrNotExist
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, apiError(resp, "download")
	}
	return resp.Body, nil
}

// List implements Backend (recursive below prefix).
func (d *GDrive) List(ctx context.Context, prefix string) ([]string, error) {
	base := strings.Trim(prefix, "/")
	id, err := d.dir(ctx, base, false)
	if err != nil || id == "" {
		return nil, err
	}
	var out []string
	var walk func(id, p string) error
	walk = func(id, p string) error {
		kids, err := d.children(ctx, id, "")
		if err != nil {
			return err
		}
		for _, k := range kids {
			kp := k.Name
			if p != "" {
				kp = p + "/" + k.Name
			}
			if k.MimeType == folderMime {
				if err := walk(k.ID, kp); err != nil {
					return err
				}
			} else {
				out = append(out, kp)
			}
		}
		return nil
	}
	return out, walk(id, base)
}

// Delete implements Backend (permanent delete of a file unth created).
func (d *GDrive) Delete(ctx context.Context, key string) error {
	id, err := d.fileID(ctx, key)
	if errors.Is(err, ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return d.deleteID(ctx, id)
}

// RemoveEmptyDir deletes the folder at dir (below the root folder) when it
// has nothing left in it. It reports whether the folder is gone.
func (d *GDrive) RemoveEmptyDir(ctx context.Context, dir string) (bool, error) {
	dir = strings.Trim(dir, "/")
	if dir == "" || dir == "." {
		return false, nil // never the root folder
	}
	id, err := d.dir(ctx, dir, false)
	if err != nil || id == "" {
		return id == "", err
	}
	kids, err := d.children(ctx, id, "")
	if err != nil || len(kids) > 0 {
		return false, err
	}
	if err := d.deleteID(ctx, id); err != nil {
		return false, err
	}
	d.mu.Lock()
	for k := range d.folders {
		if k == dir || strings.HasPrefix(k, dir+"/") {
			delete(d.folders, k)
		}
	}
	d.mu.Unlock()
	return true, nil
}

func (d *GDrive) deleteID(ctx context.Context, id string) error {
	resp, err := d.do(ctx, http.MethodDelete, d.g.DriveURL+"/files/"+url.PathEscape(id), nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return apiError(resp, "delete")
	}
	return nil
}
