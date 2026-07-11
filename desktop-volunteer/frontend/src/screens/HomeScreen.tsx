// Home tab: the big start/stop control, plain-language status for each phase,
// the live stats grid while the relay runs, and a collapsible console.
import { useEffect, useState } from 'react';
import { ConsolePanel } from '../components/ConsolePanel';
import { errorMessage } from '../core/errors';
import { formatBytes, formatDuration } from '../core/format';
import { isMock } from '../native/VolunteerService';
import type { VolunteerPhase, VolunteerState } from '../native/types';

interface Props {
  state: VolunteerState;
  onStart: () => Promise<void>;
  onStop: () => Promise<void>;
}

const PHASE_TITLE: Record<VolunteerPhase, string> = {
  idle: 'Start volunteering',
  starting: 'Getting ready\u2026',
  probing: 'Checking how your computer can be reached\u2026',
  registering: 'Joining the network\u2026',
  online: "You're helping people connect",
  retrying: 'Connection problem \u2014 retrying automatically',
  stopping: 'Stopping\u2026',
};

const SPINNER_PHASES: ReadonlySet<VolunteerPhase> = new Set([
  'starting',
  'probing',
  'registering',
  'stopping',
]);

function PowerIcon({ size }: { size: number }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
    >
      <path d="M18.36 6.64a9 9 0 1 1-12.73 0" />
      <line x1="12" y1="2" x2="12" y2="12" />
    </svg>
  );
}

export function HomeScreen({ state, onStart, onStop }: Props) {
  const [consoleOpen, setConsoleOpen] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const [nowMs, setNowMs] = useState(() => Date.now());

  // "Online for" ticks client-side once a second while the relay is online.
  useEffect(() => {
    if (state.startedAtMs <= 0) {
      return;
    }
    setNowMs(Date.now());
    const id = setInterval(() => setNowMs(Date.now()), 1000);
    return () => clearInterval(id);
  }, [state.startedAtMs]);

  const phase = state.phase;
  const isOnline = phase === 'online';
  const isRetrying = phase === 'retrying';
  const showSpinner = SPINNER_PHASES.has(phase);
  const powerDisabled = (!state.xrayFound && !state.running) || phase === 'stopping';

  const toggle = async () => {
    setActionError(null);
    try {
      if (state.running) {
        await onStop();
      } else {
        await onStart();
      }
    } catch (err) {
      setActionError(errorMessage(err));
    }
  };

  const powerClass = [
    'vol-power',
    isOnline ? 'is-online' : '',
    showSpinner ? 'is-working' : '',
    isRetrying ? 'is-retrying' : '',
  ]
    .filter(Boolean)
    .join(' ');

  return (
    <div className="vol-home">
      <header className="vol-home-header">
        <div className="vol-home-header-left">
          <div className="or-wordmark-row">
            <span className="or-wordmark">OpenRung</span>
            <span className="or-cursor">{'\u258D'}</span>
          </div>
          <span className="or-tagline">volunteer relay</span>
        </div>
        {isMock && <span className="mock-badge">mock</span>}
      </header>

      {!state.xrayFound && (
        <div className="vol-banner">
          The xray engine that powers your relay was not found. Reinstall OpenRung Volunteer.
        </div>
      )}

      <div className="vol-center">
        <button
          type="button"
          data-testid="power-button"
          className={powerClass}
          onClick={() => void toggle()}
          disabled={powerDisabled}
          aria-label={state.running ? 'Stop volunteering' : 'Start volunteering'}
        >
          {showSpinner && <span className="vol-power-spinner" aria-hidden />}
          <PowerIcon size={64} />
        </button>

        <div className="vol-status">
          <span className={`vol-status-title ${isRetrying ? 'is-retrying' : ''}`}>
            {PHASE_TITLE[phase]}
          </span>
          {phase === 'idle' && (
            <span className="vol-status-sub">
              Share your connection to help people reach the open internet.
            </span>
          )}
          {isRetrying && state.lastError != null && (
            <span className="vol-status-error">{state.lastError}</span>
          )}
          {actionError != null && <span className="vol-status-error">{actionError}</span>}
        </div>

        {isOnline && (
          <div className="vol-chip-row">
            <span className="vol-chip is-name">{state.relayLabel}</span>
            {state.transport !== '' && (
              <span className="vol-chip">
                {state.transport === 'direct' ? 'Direct connection' : 'Via relay hub'}
              </span>
            )}
          </div>
        )}

        {state.running && (
          <div className="vol-stats">
            <div className="vol-stat">
              <span className="vol-stat-value">{state.activeConnections}</span>
              {/* Not "people": one person's device opens many parallel
                  connections (a page pulls from dozens of hosts at once), and
                  in tunnel mode the hub multiplexes every client so the relay
                  only ever sees a stream count, never distinct people. */}
              <span className="vol-stat-label">Active connections</span>
            </div>
            <div className="vol-stat">
              <span className="vol-stat-value">{state.totalConnections}</span>
              <span className="vol-stat-label">Connections served</span>
            </div>
            <div className="vol-stat">
              <span className="vol-stat-value">
                {formatBytes(state.bytesFromClients + state.bytesToClients)}
              </span>
              <span className="vol-stat-label">Data shared</span>
            </div>
            <div className="vol-stat">
              <span className="vol-stat-value">
                {state.startedAtMs > 0
                  ? formatDuration(Math.max(0, nowMs - state.startedAtMs))
                  : '\u2014'}
              </span>
              <span className="vol-stat-label">Online for</span>
            </div>
          </div>
        )}
      </div>

      <div className="vol-console">
        <button
          type="button"
          className="vol-console-toggle"
          onClick={() => setConsoleOpen(open => !open)}
        >
          <span>CONSOLE</span>
          <span>{consoleOpen ? '\u25BE' : '\u25B8'}</span>
        </button>
        {consoleOpen && <ConsolePanel lines={state.logLines} />}
      </div>
    </div>
  );
}
