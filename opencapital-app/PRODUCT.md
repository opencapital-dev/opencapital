# Product

## Register

product

## Users

Traders, quants, and financial analysts who run OpenCapital as a desktop app
(Tauri) to spin up per-workspace Grafana instances over their own market /
tick data. They are technical, comfortable with dashboards, and impatient: the
shell is the lobby they pass through on the way to the real work (a Grafana
window). Context of use: focused desktop session, often dark room / long hours,
SSO already trusted. The job-to-be-done is "pick a workspace and launch its
Grafana fast, with zero friction and full confidence it worked."

## Product Purpose

OpenCapital is the launcher and control surface that sits in front of the
market-data Grafana instances it provisions. It exists so a user never touches
config: it authenticates (Kinde SSO), lists their workspaces (orgs), reconciles
plugins + provisioning, and boots Grafana in a dedicated window. Success = the
launch flow feels instant, legible, and trustworthy, and the shell looks like a
deliberate product rather than scaffolding around someone else's UI.

## Brand Personality

Confident, modern, quietly premium. Three words: **precise, assured, calm.**
The product is a serious financial tool, so the voice is plainspoken and
competent — never playful or noisy. It should read as a polished SaaS desktop
app, not a dev tool. The shell carries the brand moment (the launch screen, the
brand mark, the "live" confirmation); the working surfaces stay restrained so
data and the Grafana windows lead.

## Anti-references

- A raw, unstyled Grafana clone — generic, "this is just Grafana's chrome."
- Cluttered trading terminals with dense toolbars and gratuitous color.
- Generic dark-SaaS-template look: gradient-text headings, neon glows
  everywhere, hero-metric cards, identical icon-card grids.
- The old "TickViewer" name anywhere — the product is **OpenCapital**.

## Design Principles

1. **Borrow Grafana's bones, not its blandness.** Build on Grafana UI tokens
   (`useStyles2`, `GrafanaTheme2`) for cohesion with the windows we launch, but
   add a deliberate custom layer (depth, rhythm, motion, brand moments) so the
   shell is unmistakably OpenCapital.
2. **The launcher is the lobby, not the destination.** Optimize for fast,
   confident passage to Grafana. Every state — idle, launching, live, empty —
   should make the user sure of what just happened and what to do next.
3. **Calm under density.** Restraint over decoration. Color and motion are
   spent where they carry meaning (launch progress, live confirmation, brand),
   not sprayed across the chrome.
4. **One identity, everywhere.** Brand mark, naming, and accent are consistent
   across login, topbar, and content. No leftover names, no stray styles.

## Accessibility & Inclusion

Target WCAG 2.1 AA. Dark mode is the default and primary theme; verify body
text ≥ 4.5:1 and large/secondary text ≥ 3:1 against Grafana's dark surfaces
(don't lean on `text.disabled` for anything readable). All motion (login glow,
launch pulse, stage transitions) needs a `prefers-reduced-motion: reduce`
fallback. Interactive shell controls need visible focus states and proper
labels (the nav rail, org switcher, user menu).
