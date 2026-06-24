import React, { useMemo } from "react";
import ReactDOM from "react-dom/client";
import { createTheme } from "@grafana/data";
import { ThemeContext, GlobalStyles } from "@grafana/ui";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import App from "./App";

// @grafana/ui <Icon> resolves sprites from `${__grafana_public_path__}build/img/icons/…`.
// Point it at the app root; the grafana-icons Vite plugin serves them there.
(globalThis as { __grafana_public_path__?: string }).__grafana_public_path__ = "/";

// gcTime Infinity: cache lives for the whole session, so navigating away from a
// view and back is instant (never an empty flash). staleTime 30s + refetch on
// window focus: served-from-cache reads still revalidate in the background, so
// the catalog keeps picking up newly published plugins without blocking the UI.
const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      gcTime: Infinity,
      staleTime: 30_000,
      refetchOnWindowFocus: true,
      retry: 1,
    },
  },
});

function Root() {
  const theme = useMemo(() => createTheme({ colors: { mode: "dark" } }), []);
  return (
    <ThemeContext.Provider value={theme}>
      <GlobalStyles />
      <QueryClientProvider client={queryClient}>
        <App />
      </QueryClientProvider>
    </ThemeContext.Provider>
  );
}

ReactDOM.createRoot(document.getElementById("root") as HTMLElement).render(
  <React.StrictMode>
    <Root />
  </React.StrictMode>,
);
