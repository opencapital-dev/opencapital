import { useState } from "react";
import { api, errMsg } from "./api";
import type { KindeProfile } from "./types";
import { Login } from "./components/Login";
import { AppShell, NavKey } from "./components/AppShell";
import { LaunchView } from "./components/LaunchView";
import { PluginsView } from "./components/PluginsView";
import { SourcesView } from "./components/SourcesView";
import { SettingsView } from "./components/SettingsView";
import { UpdatePrompt } from "./components/UpdatePrompt";
import { useUpdater } from "./updater";
import "./App.css";

function App() {
  const [loggedIn, setLoggedIn] = useState(false);
  const [profile, setProfile] = useState<KindeProfile | null>(null);
  const [nav, setNav] = useState<NavKey>("launch");
  const [busy, setBusy] = useState(false);
  const [loginError, setLoginError] = useState("");
  const updater = useUpdater();
  const [promptDismissed, setPromptDismissed] = useState(false);

  async function login() {
    setBusy(true);
    setLoginError("");
    try {
      await api.kindeLogin();
      // Best-effort: a missing/failed profile shouldn't block the session.
      try {
        setProfile(await api.meProfile());
      } catch {
        setProfile(null);
      }
      setLoggedIn(true);
      // Best-effort update check on launch; never blocks usage.
      void updater.checkForUpdate();
    } catch (e) {
      setLoginError(errMsg(e));
    } finally {
      setBusy(false);
    }
  }

  async function logout() {
    try {
      await api.logout();
    } finally {
      setLoggedIn(false);
      setProfile(null);
      setNav("launch");
    }
  }

  if (!loggedIn) {
    return (
      <Login
        busy={busy}
        error={loginError}
        onLogin={login}
        onDismissError={() => setLoginError("")}
      />
    );
  }

  const userEmail = profile?.preferred_email ?? "";
  const displayName = userEmail || "User";

  return (
    <>
      <AppShell
        displayName={displayName}
        nav={nav}
        busy={busy}
        onNav={setNav}
        onLogout={logout}
      >
        {nav === "launch" && (
          <LaunchView userEmail={userEmail} />
        )}
        {nav === "plugins" && (
          <PluginsView />
        )}
        {nav === "sources" && <SourcesView />}
        {nav === "settings" && (
          <SettingsView
            state={updater.state}
            onCheck={updater.checkForUpdate}
            onInstall={updater.installAndRelaunch}
          />
        )}
      </AppShell>

      {!promptDismissed && (
        <UpdatePrompt
          state={updater.state}
          onInstall={updater.installAndRelaunch}
          onDismiss={() => setPromptDismissed(true)}
        />
      )}
    </>
  );
}

export default App;
