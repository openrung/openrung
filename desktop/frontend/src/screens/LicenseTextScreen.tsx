// Full license texts screen (desktop), ported from the RN LicenseTextScreen:
// header + one scrollable column rendering the bundled notices (third-party
// summary first, complete GNU GPL-3.0 text at the end) in small mono type.
import { GPL_TEXT, THIRD_PARTY_TEXT } from '../licenses/notices';

interface Props {
  onBack: () => void;
}

export function LicenseTextScreen({ onBack }: Props) {
  return (
    <div className="or-screen">
      <div className="or-screen-header">
        <button type="button" className="or-back-btn" onClick={onBack} aria-label="Back">
          ←
        </button>
        <h1 className="or-screen-title">Full license texts</h1>
      </div>

      <pre className="or-license-text">{THIRD_PARTY_TEXT}</pre>
      <pre className="or-license-text">{GPL_TEXT}</pre>
    </div>
  );
}
