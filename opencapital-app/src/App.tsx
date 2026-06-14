import { useState } from "react";
import { api, errMsg } from "./api";
import type { KindeProfile, MeOrgs, Org } from "./types";
import { Login } from "./components/Login";
import { AppShell, NavKey } from "./components/AppShell";
import { LaunchView } from "./components/LaunchView";
import { PluginsView } from "./components/PluginsView";
import { SourcesView } from "./components/SourcesView";
import { CreateWorkspaceModal } from "./components/CreateWorkspaceModal";
import { SettingsView } from "./components/SettingsView";
import { UpdatePrompt } from "./components/UpdatePrompt";
import { useUpdater } from "./updater";
import "./App.css";

function App() {
  const [me, setMe] = useState<MeOrgs | null>(null);
  const [profile, setProfile] = useState<KindeProfile | null>(null);
  const [selectedOrg, setSelectedOrg] = useState<Org | null>(null);
  const [nav, setNav] = useState<NavKey>("launch");
  const [busy, setBusy] = useState(false);
  const [loginError, setLoginError] = useState("");
  const [showCreate, setShowCreate] = useState(false);
  const updater = useUpdater();
  const [promptDismissed, setPromptDismissed] = useState(false);

  async function login() {
    setBusy(true);
    setLoginError("");
    try {
      await api.kindeLogin();
      const orgs = await api.meOrgs();
      setMe(orgs);
      setSelectedOrg(orgs.orgs[0] ?? null);
      // Best-effort: a missing/failed profile shouldn't block the session.
      try {
        setProfile(await api.meProfile());
      } catch {
        setProfile(null);
      }
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
      setMe(null);
      setProfile(null);
      setSelectedOrg(null);
      setNav("launch");
    }
  }

  async function reloadOrgs() {
    const orgs = await api.meOrgs();
    setMe(orgs);
    const next =
      orgs.orgs.find((o) => o.org_id === selectedOrg?.org_id) ||
      orgs.orgs[0] ||
      null;
    setSelectedOrg(next);
  }

  async function createOrg(name: string, currency: string) {
    await api.createOrg(name, currency);
    await reloadOrgs();
  }

  if (!me) {
    return (
      <Login
        busy={busy}
        error={loginError}
        onLogin={login}
        onDismissError={() => setLoginError("")}
      />
    );
  }

  // Prefer the real Kinde email; fall back to control-plane email. Empty when
  // neither is available — the Grafana backend then falls back to the subject
  // id, which we avoid surfacing as an "email".
  const userEmail = profile?.preferred_email || me.email || "";
  // Display: same, but show the subject id as a last resort.
  const displayName = userEmail || me.user_id;

  return (
    <>
      <AppShell
        displayName={displayName}
        orgs={me.orgs}
        selectedOrg={selectedOrg}
        nav={nav}
        busy={busy}
        onNav={setNav}
        onSelectOrg={setSelectedOrg}
        onCreateOrg={() => setShowCreate(true)}
        onLogout={logout}
      >
        {selectedOrg && nav === "launch" && (
          <LaunchView key={selectedOrg.org_id} me={me} org={selectedOrg} userEmail={userEmail} />
        )}
        {selectedOrg && nav === "plugins" && (
          <PluginsView key={selectedOrg.org_id} org={selectedOrg} />
        )}
        {selectedOrg && nav === "sources" && <SourcesView />}
        {selectedOrg && nav === "settings" && (
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

      <CreateWorkspaceModal
        isOpen={showCreate}
        onDismiss={() => setShowCreate(false)}
        onCreate={createOrg}
      />
    </>
  );
}

export default App;
