package connections

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/unmadethings/unthrecover/internal/security"
)

// Connection is one connected account. RefreshToken never leaves the server:
// it is not returned by the API and is redacted from logs.
type Connection struct {
	Name         string    `json:"name"`
	Provider     string    `json:"provider"` // gdrive
	Account      string    `json:"account"`  // e-mail shown in the panel
	ClientID     string    `json:"client_id"`
	RefreshToken string    `json:"refresh_token"`
	FolderID     string    `json:"folder_id,omitempty"` // where copies go: "Unth Recover Backups" or a picked folder
	FolderName   string    `json:"folder_name,omitempty"`
	FolderPicked bool      `json:"folder_picked,omitempty"` // chosen by the user with Google's picker
	CreatedAt    time.Time `json:"created_at"`
}

// Public is the connection without secrets.
type Public struct {
	Name         string    `json:"name"`
	Provider     string    `json:"provider"`
	Account      string    `json:"account"`
	Folder       string    `json:"folder"`        // folder name on Drive
	FolderPicked bool      `json:"folder_picked"` // false: the default "Unth Recover Backups"
	CreatedAt    time.Time `json:"created_at"`
}

// DefaultFolderName is the folder unth creates when none was picked.
const DefaultFolderName = "Unth Recover Backups"

// Public strips secrets.
func (c *Connection) Public() Public {
	folder := c.FolderName
	if !c.FolderPicked || folder == "" {
		folder = DefaultFolderName
	}
	return Public{Name: c.Name, Provider: c.Provider, Account: c.Account, Folder: folder, FolderPicked: c.FolderPicked, CreatedAt: c.CreatedAt}
}

// NamePattern restricts connection names (used in job files and file names).
var NamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// ErrNotFound: no such connection.
var ErrNotFound = errors.New("connection not found")

// Store keeps connections in <data-dir>/connections/<name>.json (0600, dir 0700).
type Store struct{ Dir string }

// NewStore returns the store of a data directory.
func NewStore(dataDir string) *Store { return &Store{Dir: filepath.Join(dataDir, "connections")} }

func (s *Store) path(name string) (string, error) {
	if !NamePattern.MatchString(name) {
		return "", fmt.Errorf("invalid connection name %q", name)
	}
	return filepath.Join(s.Dir, name+".json"), nil
}

// Save writes a connection atomically.
func (s *Store) Save(c *Connection) error {
	p, err := s.path(c.Name)
	if err != nil {
		return err
	}
	if err := security.EnsurePrivateDir(s.Dir); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ") // #nosec G117 -- the token is stored on purpose, in a 0600 file in a 0700 dir
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Load reads one connection (with its secret).
func (s *Store) Load(name string) (*Connection, error) {
	p, err := s.path(name)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s (connect it in the panel: Settings → Connections)", ErrNotFound, name)
	}
	raw, err := security.ReadSecretFile(p)
	if err != nil {
		return nil, err
	}
	var c Connection
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return nil, fmt.Errorf("connection %s: %w", name, err)
	}
	return &c, nil
}

// List returns all connections without secrets.
func (s *Store) List() ([]Public, error) {
	ents, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return []Public{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Public{}
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || filepath.Ext(n) != ".json" {
			continue
		}
		c, err := s.Load(n[:len(n)-5])
		if err != nil {
			continue
		}
		out = append(out, c.Public())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Delete removes a connection file.
func (s *Store) Delete(name string) error {
	p, err := s.path(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	return nil
}

// NextName returns a free name like gdrive, gdrive-2, ...
func (s *Store) NextName(base string) string {
	for i := 1; ; i++ {
		n := base
		if i > 1 {
			n = fmt.Sprintf("%s-%d", base, i)
		}
		if p, _ := s.path(n); p != "" {
			if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
				return n
			}
		}
	}
}
