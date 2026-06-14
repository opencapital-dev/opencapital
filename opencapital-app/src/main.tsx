import React, { useMemo } from "react";
import ReactDOM from "react-dom/client";
import { createTheme } from "@grafana/data";
import { ThemeContext, GlobalStyles } from "@grafana/ui";
import App from "./App";

// @grafana/ui <Icon> resolves sprites from `${__grafana_public_path__}build/img/icons/…`.
// Point it at the app root; the grafana-icons Vite plugin serves them there.
(globalThis as { __grafana_public_path__?: string }).__grafana_public_path__ = "/";

function Root() {
  const theme = useMemo(() => createTheme({ colors: { mode: "dark" } }), []);
  return (
    <ThemeContext.Provider value={theme}>
      <GlobalStyles />
      <App />
    </ThemeContext.Provider>
  );
}

ReactDOM.createRoot(document.getElementById("root") as HTMLElement).render(
  <React.StrictMode>
    <Root />
  </React.StrictMode>,
);
