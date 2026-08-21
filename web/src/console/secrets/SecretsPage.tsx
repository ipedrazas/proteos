import { ClaudeSection } from './ClaudeSection';
import { GitHostsSection } from './GitHostsSection';
import { GitIdentitySection } from './GitIdentitySection';
import { ProvidersSection } from './ProvidersSection';
import { TokensSection } from './TokensSection';

// SecretsPage is every credential your machines act with, on one page.
//
// One page with sections rather than the desktop's tabs-within-a-window: these
// are five different things with different lifecycles, and a reader arriving
// with "is anything about to expire?" should be able to answer it by scrolling
// rather than by opening five tabs in turn.
//
// One rule holds across all of them: nothing here is ever rendered back after
// it is submitted. The API returns only whether a credential is set, never its
// value. The single exception is a freshly minted access token, which is shown
// exactly once because the control plane stores only a hash — see TokensSection.
export function SecretsPage() {
  return (
    <div className="c-page">
      <header className="c-page-header">
        <h1 className="c-page-title">Secrets</h1>
        <p className="c-muted c-page-sub">
          Credentials your machines use. Stored encrypted, injected at runtime, and never shown
          again once saved.
        </p>
      </header>

      <ProvidersSection />
      <ClaudeSection />
      <TokensSection />
      <GitHostsSection />
      <GitIdentitySection />
    </div>
  );
}
