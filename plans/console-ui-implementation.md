# Console UI ("AWS-like") — Implementation Plan

> Source: user request, 2026-08-21 — "create an alternative UI to the current one … more
> AWS-like and less desktop-like … run side by side."
> Status: **not started.**
>
> Scope locked with the user: create VMs, see status, add secrets and SSH keys,
> Start/Stop, Create/Destroy, and launch web terminals into those VMs.
> Non-goal: replacing or modifying `web/src/desktop/*` or `web/src/mobile/*`.

---

## 1. Context — what already exists

The backend needs **no changes for the locked scope**. Everything the console must do is
already a route on the control plane, and everything it must render is already a type in
`web/src/api/client.ts`. This is a front-end-only piece of work.

**Reusable as-is (do not fork):**

| Layer | File | Notes |
|---|---|---|
| Typed API client | `web/src/api/client.ts` (1119 lines) | CSRF header, `SessionExpiredError`, every DTO |
| React Query hooks | `web/src/api/hooks.ts` (897 lines) | `useMachines`, `useMachineMutations`, `useMachineEvents`, `useTemplates`, `useProviders`, `useSSHLoginKeys`, `useTokens`, … |
| Terminal transport | `web/src/lib/terminalSocket.ts` | reconnect/backoff, close-code semantics, `terminalURL`/`agentURL` |
| Terminal widget | `web/src/components/Terminal.tsx` | xterm + FitAddon, already shell-agnostic (props: `machineID`, `provider?`, `session?`, `cwd?`) |
| Logging | `web/src/lib/logger.ts`, `lib/uiLogReporter.ts` | installed once in `main.tsx` |
| Auth gate | `web/src/App.tsx` `RequireAuth` | `/api/me` → login redirect → GitHub-connect gate |

**Precedent for a second shell already exists.** `web/src/mobile/` is a purpose-built
alternative shell mounted at `/m`, sharing the same api/hooks layer, with all class names
namespaced `m-` in its own `mobile.css`. The console is the same pattern at a third scale.
`web/src/mobileGate.ts` is the precedent for a remembered UI preference in `localStorage`.

**What is genuinely missing** (front-end only): a resource-oriented navigation model, dense
tables, a full-page launch wizard, detail pages with tabs, and bulk actions. The desktop UI
expresses all of this as draggable windows over a wallpaper, which is the thing users are
reacting to.

---

## 2. Decision — where the new UI lives

**Chosen: a new route tree in the same Vite app.** `web/src/console/`, mounted at
`/console/*`, sharing `src/api`, `src/lib`, and `src/components/Terminal.tsx`.

Rationale:
- The session-cookie + CSRF model requires the SPA and the API to share one origin. A
  second origin/container means re-solving cookies, CSRF, SSE, and the WebSocket gateway's
  Origin allowlist for no product gain.
- `nginx.conf`'s `try_files $uri $uri/ /index.html` already serves `/console/*`. Vite's dev
  proxy already covers `/api` and `/gw`. **Zero infrastructure change.**
- The 2000-line typed API layer is not duplicated, so an API change lands once.
- CI (`task web:ci`) covers the new code automatically.

**Alternatives considered and rejected:**
- *Separate `webconsole/` package + second nginx container.* True isolation, but forks
  `client.ts`/`hooks.ts`, doubles CI, needs a new compose service, a new nginx `location`,
  and an origin decision. Revisit only if the console is ever to be sold/deployed alone.
- *Vite multi-page build (`console.html` entry).* Real bundle isolation, one container —
  but needs an nginx `location /console { try_files $uri /console.html; }` rule and a
  second `rollupOptions.input`. Route-level `React.lazy` (below) gets ~90% of the bundle
  benefit for none of the infra cost.

**Bundle isolation:** the console tree is loaded with `React.lazy(() => import('./console/ConsoleApp'))`
so desktop-only users never download it, and vice versa.

---

## 3. Information architecture

AWS-shaped: a persistent left service nav, a resource **list** page per service, a resource
**detail** page with tabs, and full-page **wizards** for creation. No windows, no wallpaper,
no dock.

```
┌───────────────────────────────────────────────────────────────┐
│ ProteOS │ Console                            [ipedrazas ▾]    │  top bar
├────────────┬──────────────────────────────────────────────────┤
│ COMPUTE    │  Machines (3/5)            [⟳] [Launch machine]  │
│  Machines  │  ┌────────────────────────────────────────────┐  │
│  Templates │  │ ☐  Name    State     Size       Age  Disk  │  │
│ ACCESS     │  │ ☐  dev-1   ● Running 2 vCPU/4Gi 3d   10Gi  │  │
│  SSH keys  │  │ ☐  dev-2   ◌ Pending 4 vCPU/8Gi 1m   20Gi  │  │
│  Secrets   │  └────────────────────────────────────────────┘  │
│ OPERATIONS │                                                  │
│  Activity  │                                                  │
│  Admin     │                                                  │
└────────────┴──────────────────────────────────────────────────┘
```

### Route map

| Route | Screen | Backing endpoints |
|---|---|---|
| `/console` | → redirect `/console/machines` | — |
| `/console/machines` | Machines list + bulk actions | `GET /api/machines`, `GET /api/machine/events` (SSE) |
| `/console/machines/launch` | Launch wizard (full page) | `GET /api/templates`, `POST /api/machines` |
| `/console/machines/:id` | → redirect `…/overview` | |
| `/console/machines/:id/overview` | Summary, resources, disk/snapshot, errors, rename | `GET /api/machines/{id}`, `PATCH /api/machines/{id}` |
| `/console/machines/:id/connect` | Connect chooser: web terminal / SSH / editor | — |
| `/console/machines/:id/terminal` | Full-page terminal, multi-tab | `GET /gw/terminal` (WS) |
| `/console/machines/:id/networking` | Network policy editor | `GET/PUT/DELETE /api/machines/{id}/network-policy` |
| `/console/machines/:id/activity` | Per-machine event feed | SSE stream, filtered client-side |
| `/console/keys` | SSH keys + account-wide SSH switch | `GET/POST/DELETE /api/ssh-keys`, `PATCH /api/user/preferences` |
| `/console/secrets` | Providers · Tokens · Git hosts · Claude · Git identity | `/api/providers`, `/api/tokens`, `/api/git/hosts`, `/api/profile/*` |
| `/console/templates` | Read-only catalog ("AMIs") | `GET /api/templates` |
| `/console/activity` | Fleet-wide event feed + app logs | SSE, `GET /api/logs` |
| `/console/admin` | Fleet console (only when `me.is_admin`) | `GET /api/admin/overview` |

### Vocabulary

Keep the API's word — **machine** — in the UI, and use it in routes too
(`/console/machines`, not `/console/instances`). Do not introduce "instance" as a second
name for the same object; the CLI, the docs, and the API all say machine. AWS-like refers
to the *interaction model* (resource lists, detail tabs, launch wizard), not to renaming
the domain out from under everything else.

---

## 4. Screen specs

### 4.1 Machines list — `/console/machines`

The centrepiece. A dense table, not cards.

- **Columns:** checkbox · Name (link to detail) · State badge · Type (`{vcpus} vCPU / {mem_mib} GiB`)
  · Disk · Template · Age (`created_at`) · Last active · ID (truncated, copy button).
- **State badges**, mapped from `MachineState`:

  | API state | Badge | Colour |
  |---|---|---|
  | `requested`, `provisioning`, `starting` | Pending | amber + spinner |
  | `running` | Running | green |
  | `stopping`, `hibernating` | Stopping | amber |
  | `stopped` | Stopped | grey |
  | `error` | Error | red, `last_error` in a tooltip and on the detail page |

  This mapping is a **pure function in its own module** (`console/machineState.ts`) with a
  unit test — it is the piece most likely to drift as states are added.
- **Toolbar:** free-text filter (name / id / template), state filter chips, refresh button,
  `Launch machine` primary button. The count reads `N of {me.machine_limit}`; the launch
  button is disabled with an explanatory tooltip at the cap.
- **Action bar** (enabled by selection): Start · Stop · Connect · Terminate.

  Start and Stop apply to the eligible **subset** of the selection, not all-or-nothing:
  select everything, hit Start, and the stopped ones start — what EC2 does and what people
  mean. The cost is ambiguity about how many machines a click touches, paid for by putting
  the count on the button whenever it is acting on less than the full selection
  (`Start (2)`). Both are non-destructive, so acting on a subset is recoverable.

  Terminate deliberately does **not** work that way: it applies to everything selected and
  the dialog names every machine before anything happens. A destructive action must never
  quietly act on a subset. Connect requires a single `running` selection.

  *(Revised in Phase 1 from the original all-or-nothing rule, which a test caught as a
  divergence from the implementation. All-or-nothing is more predictable but makes a
  select-all-then-Start impossible, which is the common case.)*
- **Bulk actions are a client-side fan-out.** There is no bulk start/stop endpoint —
  only `DELETE /api/machines` (destroy *all*, not a subset) and `POST /api/machines/fill`.
  Fan out individual calls with a concurrency cap of 4, render per-row progress in the
  table, and report a per-row success/failure summary. Do **not** call the destroy-all
  endpoint for a partial selection.
- **Liveness:** `useMachineEvents()` already writes SSE state straight into the
  `['machines']` query cache. The table re-renders from cache; no polling.
- **Empty state:** a first-run panel explaining what a machine is, with the launch CTA —
  not a blank table.

### 4.2 Launch wizard — `/console/machines/launch`

Full-page, stepped, EC2-launch-wizard shape. Four steps with a persistent right-hand summary:

1. **Name** — optional; blank means server-generated. Inline note.
2. **Template** — cards from `GET /api/templates` (`label`, `description`, default resources).
3. **Size** — vCPU / memory / disk sliders + numeric inputs, clamped live to
   `template.limits`. Preselected from `template.defaults`; changing template resets them
   (existing `CreateMachineDialog` behaviour — keep it).
4. **Review and launch** — full summary, then `POST /api/machines`.

Error codes to surface inline, not as a toast: `machine_limit`, `unknown_template`,
`invalid_resources` (show `err.detail`, e.g. "vcpus must be 1..8"). On success, navigate to
`/console/machines/:id/overview`, which will show `Pending` until SSE flips it to Running.

### 4.3 Machine detail — `/console/machines/:id/*`

Header: name (inline-editable → `PATCH /api/machines/{id}`), state badge, ID with copy, and
the action buttons (Start / Stop / Connect / Terminate) with the same enablement rules.

- **Overview:** two-column definition list — state, template, vCPU/memory/disk, guest IP,
  boot mode (`cold`/`resumed`), disk id, snapshot metadata (`fc_version`, `mem_bytes`,
  `created_at`) when hibernated, created/last-active, and `last_error` in a red callout
  when non-null.
- **Connect:** three methods, tabbed — **Web terminal** (in-console, the default), **SSH
  client** (the `ProxyCommand` instructions, reusing the copy from `SSHAccessPanel`, with a
  live warning when `me.prefs.ssh_enabled` is false and a link to `/console/keys`), and
  **Editor** (mints a one-shot web-session URL via `POST /api/machine/web-session` and opens
  code-server in a new tab).
- **Terminal:** full-page xterm filling the content area, with a tab strip for multiple
  sessions (each tab = a distinct `session` id passed to `<Terminal>`). A `stopped` machine
  shows a "Start this machine to connect" panel instead — the gateway returns 409
  `machine_not_running` and the browser WebSocket API hides that status.
- **Networking:** network-policy mode radio (`allow_all` / `deny_all` / `allow_domains` /
  `deny_domains`) + domain list editor.
- **Activity:** this machine's events from the SSE ring, newest first, with state
  transitions rendered as `from → to`.

**Terminate** reuses the existing guarded flow's semantics: confirm → server-side session
export → on `session_export_failed`, offer "Force delete anyway" (`force: true`). Port the
state machine from `DestroyMachineDialog.tsx`; do not re-derive it.

### 4.4 SSH keys — `/console/keys`

- The **account-wide SSH switch** (`me.prefs.ssh_enabled` via `PATCH /api/user/preferences`),
  presented as the kill-switch it is: turning it off drops `authorized_keys` from every
  machine *and* refuses the tunnel. Keep the existing confirm copy.
- **Registered public keys** table: label, fingerprint, added date, revoke.
  Add-key form validates shape client-side before `POST /api/ssh-keys`, and the label
  defaults to the key's own comment (`ssh-keygen` writes `user@host`).

  The validation's real job is catching a pasted **private** key. The server rejects one
  too, but only after the secret has crossed the network, passed a TLS terminator and a
  reverse proxy, and possibly landed in an access log. The browser is the only place it is
  still private, so that check runs first and is phrased as an instruction, not a
  validation message.
- **Managed git key** (`GET/POST/DELETE /api/profile/ssh`) — a separate section, clearly
  labelled as the *outbound* key the guest pushes to git hosts with. These two key concepts
  are already confusable; the console must separate them visually and say which is which.
- Connection instructions with the `ProxyCommand` snippet and a copy button.

### 4.5 Secrets — `/console/secrets`

Tabbed, all write-only surfaces, all with an explicit "never shown again" statement:

- **AI providers** — data-rendered form from `provider.secret_fields`; `Key set` / `No key` badge.
- **Access tokens** — mint (plaintext shown exactly once, with copy), list by prefix, revoke.
- **Git hosts** — allowlisted hosts, PAT per host, linked login.
- **Claude subscription** — the profile-item OAuth token, with `needs_reconnect` surfaced.
- **Git identity** — name/email, showing whether the effective source is `profile` or `github`.

### 4.6 Templates, Activity, Admin

Thin: a read-only template catalog table; a fleet-wide event feed joined with
`GET /api/logs`; and the admin fleet overview (totals + machine table + the `truncated`
caveat, which must say the *table* is partial, not the numbers). Admin nav renders only when
`me.is_admin`, and the server re-checks the role on every `/api/admin` route regardless.

---

## 5. Entry, switching, and coexistence

- `App.tsx` gains one route: `/console/*` → `RequireAuth` → lazy `<ConsoleApp me={me} />`.
  The desktop and mobile routes are untouched.
- Both shells get a **UI switcher** in their account menu: "Switch to Console" / "Switch to
  Desktop". The choice is remembered in `localStorage` under a new key
  (`proteos-ui-shell`), following `mobileGate.ts`'s pattern, and `RootGate` consults it
  **after** the existing phone check so mobile behaviour is unchanged.
- Default stays **desktop** through Phase 6. Flipping the default is a separate,
  explicit decision (Phase 7) once the console covers the locked scope.
- `?shell=console` / `?shell=desktop` are the URL escape hatches, mirroring `?desktop=1`.

---

## 6. Backend gaps (flagged, not scheduled)

None block the locked scope. Recorded so they are not discovered mid-build:

1. **No reboot endpoint.** AWS has one. Either omit Reboot or implement it client-side as
   stop-then-start with honest progress copy. Recommendation: omit in v1 — a client-side
   composite that fails halfway is worse than its absence.
2. **No bulk start/stop.** Handled by client fan-out (§4.1). If bulk becomes a routine
   operation, a real batch endpoint is the fix — not more client concurrency.
3. **No per-machine metrics.** A "Monitoring" tab with CPU/memory graphs is the most
   AWS-like thing the console *cannot* build today; there is no time-series API. Out of
   scope, worth its own plan.
4. **No tags.** AWS-style tag filtering has no backing store. `name` + template are the
   only filter dimensions available.

---

## 7. Phasing (tracer bullets)

Each phase ends green on `task web:ci` and is independently mergeable behind the fact that
nothing routes to `/console` by default.

**Phase 0 — Shell (skeleton that renders).**
Route, lazy entry, `ConsoleApp`, top bar, left nav, content outlet, `console.css` token
block, account menu with the shell switcher. Every service page is a titled empty panel.
*Done when:* `/console` loads authenticated, nav highlights the active route, switching back
to the desktop works and is remembered.

**Phase 1 — Machines list + lifecycle.** ← the tracer bullet that proves the model
Table, state badges (`machineState.ts` + test), filters, row actions Start/Stop/Terminate,
bulk fan-out with per-row progress, SSE liveness, empty state, machine-limit counter.
*Done when:* a machine started from the console reaches Running without a manual refresh,
and terminate handles the `session_export_failed` → force path.

**Phase 2 — Launch wizard + detail Overview.**
Four-step wizard, live limit clamping, the three inline error codes; Overview, inline
rename, `last_error` callout.
*Done when:* a machine can be created from the console with a non-default template and
overridden resources, and the resulting detail page shows exactly what was requested.

*Shipped without the tab strip.* Overview was the only tab that existed, and a strip of
three "not built yet" tabs is worse than no strip. The URLs were already tabbed, so Phase 3
added the siblings without changing a link. **Landed in Phase 3.**

**Phase 3 — Connect + web terminal.**
Connect chooser, full-page terminal with a multi-session tab strip, the stopped-machine
guard, the editor hand-off via one-shot web session.
*Done when:* two terminal tabs on one machine hold independent sessions and survive a
reconnect (kill the socket in devtools; scrollback replays without duplication).

*Known limitation.* Leaving the Terminal tab unmounts it, dropping its sockets; returning
reopens `main` only, so extra shells are forgotten by the UI. Session ids are deterministic,
so `main` resumes its own PTY — but a second shell has to be reopened. Keeping the tab
mounted across the whole detail page would fix it and is deliberately not done here: it
means rendering every tab at once, which is a bigger structural change than this phase
warrants. Revisit if people actually keep several shells open.

**Phase 4 — SSH keys.**
SSH master switch with the kill-switch confirm, login-key table + add/revoke, the managed
git key section, connection instructions.
*Done when:* a key added in the console lets a real `ssh` in, and toggling the switch off
visibly blocks it.

**Phase 5 — Secrets.**
All five tabs. Write-only discipline everywhere; the one-time token reveal.
*Done when:* a provider key set from the console makes an agent run on a machine.

**Phase 6 — Networking, Activity, Templates, Admin, polish.**
The remaining tabs plus: keyboard navigation on the table, focus management in dialogs,
`aria-*` on tabs/badges, responsive collapse of the left nav below 1024px, and a pass on
loading/empty/error states across every screen.

**Phase 7 — Default-shell decision + docs.**
Decide whether `/` defaults to the console. Update `README.md` and `docs/architecture.md`
with the three-shell model. Optionally add a per-user preference server-side.

---

## 8. File manifest

```
web/src/console/
  ConsoleApp.tsx           route tree + layout (lazy entry point)
  console.css              all c- namespaced styles, scoped under .c-app
  nav.tsx                  left-nav model (single source for items + admin gating)
  TopBar.tsx               brand, account menu, shell switcher
  Placeholder.tsx          Phase 0 stand-in page; deleted route by route
  ui/                      the console's primitives — used everywhere, owned here
    DataTable.tsx          selection, sort, empty/loading states
    StateBadge.tsx
    Toolbar.tsx  Tabs.tsx  Dialog.tsx  CopyButton.tsx  Field.tsx
  machineState.ts          MachineState → badge/label/action-enablement (pure, tested)
  machineState.test.ts
  bulk.ts                  concurrency-capped fan-out + per-item result (pure, tested)
  bulk.test.ts
  machines/
    MachinesList.tsx  LaunchWizard.tsx  MachineDetail.tsx
    OverviewTab.tsx  ConnectTab.tsx  TerminalTab.tsx  NetworkingTab.tsx  ActivityTab.tsx
    TerminateDialog.tsx
  keys/KeysPage.tsx
  secrets/SecretsPage.tsx  (+ per-tab sections)
  templates/TemplatesPage.tsx
  activity/ActivityPage.tsx
  admin/AdminPage.tsx
```

Touched outside `console/` (all of it landed in Phase 0):

- `web/src/App.tsx` — one lazy `/console/*` route, and the shell branch in `RootGate`.
- `web/src/shellChoice.ts` (+ test) — the remembered preference. Separate from
  `mobileGate.ts` rather than folded into it: they answer different questions (viewport vs
  explicit choice) and mobileGate must keep running first.
- `web/src/lib/useClickOutside.ts` — moved up from `desktop/`. It is a generic DOM hook and
  both shells need it; importing `console/ → desktop/` would be the wrong dependency edge.
- `web/src/desktop/LeftRail.tsx` — the "Switch to console UI" account-menu item, plus the
  moved import.

**Nothing else in `desktop/` or `mobile/` changes.**

---

## 9. Design language

Not a new design system — the same `:root` palette in `styles.css` (`--bg #0d1117`,
`--panel #161b22`, `--border #30363d`, `--text`, `--muted`, `--accent #2f81f7`, `--error`),
with console-specific tokens layered on top of `.c-app`:

- Density: 32px table rows, 13px base text in tables, 8px spacing unit.
- Surfaces: flat panels with 1px borders and 6px radii — no shadows, no rounded windows,
  no drag affordances. The visual message is "control panel", not "desktop".
- Status colours: green `#3fb950`, amber `#d29922`, red `--error`, grey `--muted`.
- Every class is `c-` prefixed and every rule is scoped under `.c-app` so the 3182-line
  global `styles.css` cannot bleed in and the console cannot bleed out.

---

## 10. Testing and gates

- The PR gate (`.github/workflows/ci.yml`, mirrored by `task web:ci`) is:
  `npm ci` → `npm audit --audit-level=high` → eslint → prettier check → `tsc --noEmit` →
  vitest → `vite build`. Adding a dependency therefore has to clear `npm audit`; the
  console adds none.
- `npm run knip` exists but is **not** wired into CI, and already reports 8 pre-existing
  findings. Treat it as hygiene to run deliberately, not as a gate.
- **Unit tests** for the pure modules only, matching house practice
  (`mobileGate.test.ts`, `windowState.test.ts`, `cloneFailure.test.ts`): `machineState.ts`
  (state → badge/label/enablement, including an unknown-state fallback), `bulk.ts`
  (concurrency cap, partial failure, ordering), and the shell-choice resolver.
- **Component smoke tests** for the table (selection maths, action enablement) using the
  existing jsdom setup.
- Manual verification per phase against a live stack (`task dev:db`), because the SSE and
  WebSocket behaviour is what actually matters and is not covered by jsdom.

---

## 11. Risks and known gotchas

1. **React StrictMode double-mount.** `main.tsx` renders under `StrictMode`; effects run
   twice in dev. The SSE stream and the terminal socket both open in effects. There is a
   precedent test (`desktop/mountOnce.test.tsx`) — follow it for any new effect that opens
   a connection.
2. **CSS bleed.** `styles.css` is global and large. Namespacing is mandatory, not stylistic.
3. **Don't pre-export future API.** `npm run knip` flags exports unreachable from the entry
   graph. It is not a CI gate (see §10) and already has pre-existing findings, so this is
   about not adding to the pile: add an export in the same change that imports it.
4. **`noUnusedLocals` / `noUnusedParameters`** are on — scaffolding with placeholder props
   will not typecheck. Build each screen to working state rather than stubbing signatures.
5. **WebSocket pre-upgrade statuses are invisible** to the browser. A stopped machine's 409
   looks identical to a network failure. Gate the terminal on machine state client-side and
   render an explicit reason, or users will read a real error as flakiness.
6. **Bundle size.** `xterm` + `react-rnd` are already dependencies; the lazy console route
   keeps the desktop bundle from growing. Verify with `vite build` output per phase.
7. **Two SSE streams.** Only one shell is mounted at a time (routes are exclusive), so
   there is no double-subscription — but keep it that way; do not hoist `useMachineEvents`
   above the route split.
8. **Write every console path absolute.** The console mounts under a splat route
   (`/console/*`), so a relative target resolves against the *matched pathname*, not the
   mount point: `<Navigate to="machines">` from `/console/nope` resolves to
   `/console/nope/machines`, which matches the splat again and redirects forever — a hard
   hang, in the browser as much as in a test. Found and fixed in Phase 0; the constraint
   matters more in Phases 2–3, which add nested detail routes and tabs. `nav.tsx` exports
   `MACHINES` as the one shared path constant; add siblings there rather than inlining
   strings.
