// Full-window informed-consent gate shown until the bridge reports
// consentAccepted. The copy deliberately mirrors Tor-exit-style honesty: the
// volunteer's IP is what destination sites see, and that trade-off must be
// acknowledged explicitly before the Start button ever becomes reachable.
import { useState } from 'react';
import { Logo } from '../components/Logo';
import { AppConfig } from '../core/config';
import { errorMessage } from '../core/errors';
import { openExternal } from '../native/openExternal';
import { isMock } from '../native/VolunteerService';
import { canAcceptConsent } from './consent';

interface Props {
  consentAccepted: boolean;
  onAccept: () => Promise<void>;
}

export function ConsentGate({ consentAccepted, onAccept }: Props) {
  const [acknowledged, setAcknowledged] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const accept = async () => {
    setError(null);
    try {
      await onAccept();
    } catch (err) {
      setError(errorMessage(err));
    }
  };

  return (
    <div className="consent-wrap">
      <div className="consent-card">
        <div className="consent-head">
          <Logo size={38} />
          <h1 className="consent-title">Before you volunteer</h1>
          {isMock && <span className="mock-badge">mock</span>}
        </div>

        <p className="consent-body">
          OpenRung volunteers share their internet connection so people in censored countries can
          reach the open internet.
        </p>

        <div className="consent-warning">
          Your computer becomes a traffic exit. Websites that people visit through your relay will
          see YOUR IP address as the source {'\u2014'} similar to running a Tor exit node. Abuse
          complaints are possible. Whether this is safe for you depends on where you live; read
          more at{' '}
          <button
            type="button"
            className="consent-link"
            onClick={() => openExternal(AppConfig.WEBSITE_URL)}
          >
            openrung.org
          </button>{' '}
          before deciding.
        </div>

        <p className="consent-body">
          While people are connected, your relay uses your internet bandwidth. The speed setting
          (default 100 Mbps) is only advertised to the network as a hint {'—'} it is{' '}
          <strong>not enforced yet</strong>, so actual usage can exceed it.
        </p>

        <p className="consent-body">
          OpenRung does not log anyone{'\u2019'}s browsing on your machine, and connection
          details of the people you help are never shown to you.
        </p>

        <label className="consent-check">
          <input
            type="checkbox"
            checked={acknowledged}
            onChange={e => setAcknowledged(e.target.checked)}
          />
          <span>
            I understand that my IP address will be visible to the sites people visit through my
            relay
          </span>
        </label>

        {error != null && <div className="consent-error">{error}</div>}

        <button
          type="button"
          className="consent-accept"
          disabled={!canAcceptConsent(acknowledged, consentAccepted)}
          onClick={() => void accept()}
        >
          I understand {'\u2014'} enable volunteering
        </button>
      </div>
    </div>
  );
}
