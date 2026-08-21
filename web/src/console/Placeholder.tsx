// Placeholder is the Phase 0 stand-in for a console service page: the real page
// header a user will keep seeing, over a panel that says plainly what is not
// built yet. It exists so the shell — routing, nav highlighting, layout,
// scrolling — can be reviewed and merged before any service page lands, and it
// is deleted service by service as the later phases fill each route in.
export function Placeholder({ title, summary }: { title: string; summary: string }) {
  return (
    <div className="c-page">
      <header className="c-page-header">
        <h1 className="c-page-title">{title}</h1>
      </header>
      <div className="c-panel c-placeholder">
        <p className="c-muted">{summary}</p>
        <p className="c-muted c-placeholder-note">
          Not built yet. Until it is, this lives in the desktop UI — switch shells from the account
          menu.
        </p>
      </div>
    </div>
  );
}
