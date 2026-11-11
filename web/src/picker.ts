// Google's folder picker. Picking a folder shares exactly that folder with
// Unth Recover (scope drive.file); the rest of the Drive stays invisible.
/* eslint-disable @typescript-eslint/no-explicit-any */
declare global {
  interface Window {
    gapi?: any;
    google?: any;
  }
}

let loading: Promise<void> | undefined;

function loadPicker(): Promise<void> {
  if (window.google?.picker) return Promise.resolve();
  loading ??= new Promise<void>((resolve, reject) => {
    const s = document.createElement("script");
    s.src = "https://apis.google.com/js/api.js";
    s.async = true;
    s.onload = () => window.gapi.load("picker", { callback: () => resolve(), onerror: () => reject(new Error("Google's picker did not load")) });
    s.onerror = () => {
      loading = undefined;
      reject(new Error("Google's picker could not be loaded (network or browser extension)"));
    };
    document.head.appendChild(s);
  });
  return loading;
}

export async function pickDriveFolder(cfg: { access_token: string; api_key: string; app_id: string }): Promise<{ id: string; name: string } | null> {
  await loadPicker();
  const gp = window.google.picker;
  const mine = new gp.DocsView(gp.ViewId.FOLDERS).setIncludeFolders(true).setSelectFolderEnabled(true).setMimeTypes("application/vnd.google-apps.folder");
  const shared = new gp.DocsView(gp.ViewId.FOLDERS).setIncludeFolders(true).setSelectFolderEnabled(true).setEnableDrives(true);
  return new Promise((resolve) => {
    const picker = new gp.PickerBuilder()
      .setTitle("Choose the folder for backup copies")
      .addView(mine)
      .addView(shared)
      .enableFeature(gp.Feature.SUPPORT_DRIVES)
      .setOAuthToken(cfg.access_token)
      .setDeveloperKey(cfg.api_key)
      .setAppId(cfg.app_id)
      .setOrigin(location.origin)
      .setCallback((data: any) => {
        if (data.action === gp.Action.PICKED && data.docs?.[0]) resolve({ id: data.docs[0].id, name: data.docs[0].name });
        else if (data.action === gp.Action.CANCEL) resolve(null);
      })
      .build();
    picker.setVisible(true);
  });
}
