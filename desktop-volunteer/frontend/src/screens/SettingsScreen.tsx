// Settings tab: relay identity and capacity, plus an advanced accordion for
// the network knobs. Everything is locked while the relay is running (the Go
// service applies settings at start), and SaveSettings rejections surface
// inline exactly as the service words them.
import { useEffect, useRef, useState } from 'react';
import { errorMessage } from '../core/errors';
import { VolunteerService } from '../native/VolunteerService';
import type { VolunteerSettings, VolunteerState } from '../native/types';

interface Props {
  state: VolunteerState;
}

type SaveStatus = { kind: 'idle' } | { kind: 'saved' } | { kind: 'error'; message: string };

// All three numeric settings (max sessions, max Mbps, listen port) map to Go
// `int` fields, so a fractional value would make the Wails JSON bridge reject
// the whole save with an opaque unmarshal error. Truncate to an integer here so
// the frontend contract matches the backend's.
function parseNumber(value: string): number {
  const n = Number(value);
  return Number.isFinite(n) ? Math.trunc(n) : 0;
}

export function SettingsScreen({ state }: Props) {
  // Seeded from the bridge snapshot on mount (App only renders tabs once the
  // store is hydrated); re-seeded from the normalized result on save.
  const [form, setForm] = useState<VolunteerSettings>(state.settings);
  const [advancedOpen, setAdvancedOpen] = useState(false);
  const [saveStatus, setSaveStatus] = useState<SaveStatus>({ kind: 'idle' });
  const [busy, setBusy] = useState(false);
  const savedTimer = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(
    () => () => {
      if (savedTimer.current != null) {
        clearTimeout(savedTimer.current);
      }
    },
    [],
  );

  const locked = state.running;
  const patch = (p: Partial<VolunteerSettings>) => setForm(f => ({ ...f, ...p }));

  const regenerate = async () => {
    try {
      const label = await VolunteerService.regenerateLabel();
      patch({ label });
    } catch (err) {
      setSaveStatus({ kind: 'error', message: errorMessage(err) });
    }
  };

  const save = async () => {
    setBusy(true);
    setSaveStatus({ kind: 'idle' });
    try {
      const saved = await VolunteerService.saveSettings(form);
      setForm(saved);
      setSaveStatus({ kind: 'saved' });
      if (savedTimer.current != null) {
        clearTimeout(savedTimer.current);
      }
      savedTimer.current = setTimeout(
        () => setSaveStatus(s => (s.kind === 'saved' ? { kind: 'idle' } : s)),
        2500,
      );
    } catch (err) {
      setSaveStatus({ kind: 'error', message: errorMessage(err) });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="or-screen">
      <h1 className="or-screen-title">Settings</h1>

      {locked && <div className="vol-form-note">Stop volunteering to change settings.</div>}

      <span className="or-section-header">RELAY</span>

      <div className="vol-field">
        <label className="vol-label" htmlFor="relay-name">
          Relay name
        </label>
        <div className="vol-inline">
          <input
            id="relay-name"
            className="vol-input"
            type="text"
            value={form.label}
            disabled={locked}
            onChange={e => patch({ label: e.target.value })}
          />
          <button
            type="button"
            className="vol-mini-button"
            disabled={locked}
            onClick={() => void regenerate()}
          >
            New random name
          </button>
        </div>
        <span className="vol-help">
          This name is public in the relay directory. Don{'\u2019'}t use your real name.
        </span>
      </div>

      <div className="vol-field">
        <label className="vol-label" htmlFor="max-sessions">
          People at once
        </label>
        <input
          id="max-sessions"
          className="vol-input is-narrow"
          type="number"
          min={1}
          max={64}
          value={form.maxSessions}
          disabled={locked}
          onChange={e => patch({ maxSessions: parseNumber(e.target.value) })}
        />
        <span className="vol-help">How many people can use your relay at the same time.</span>
      </div>

      <div className="vol-field">
        <label className="vol-label" htmlFor="max-mbps">
          Speed target (Mbps)
        </label>
        <input
          id="max-mbps"
          className="vol-input is-narrow"
          type="number"
          min={1}
          max={10000}
          value={form.maxMbps}
          disabled={locked}
          onChange={e => patch({ maxMbps: parseNumber(e.target.value) })}
        />
        <span className="vol-help">Advertised to the network to steer load. Not a strict cap yet.</span>
      </div>

      <span className="or-section-header">ADVANCED</span>

      <button
        type="button"
        className="vol-accordion-toggle"
        onClick={() => setAdvancedOpen(open => !open)}
      >
        <span>Network options</span>
        <span className="or-setting-chevron">{advancedOpen ? '\u25BE' : '\u25B8'}</span>
      </button>

      {advancedOpen && (
        <>
          <div className="vol-field">
            <label className="vol-label" htmlFor="listen-port">
              Listen port
            </label>
            <input
              id="listen-port"
              className="vol-input is-narrow"
              type="number"
              min={1}
              max={65535}
              value={form.listenPort}
              disabled={locked}
              onChange={e => patch({ listenPort: parseNumber(e.target.value) })}
            />
            <span className="vol-help">
              The port your relay accepts direct connections on.
            </span>
          </div>

          <div className="vol-field">
            <label className="vol-label" htmlFor="broker-url">
              Broker URL
            </label>
            <input
              id="broker-url"
              className="vol-input"
              type="text"
              value={form.brokerUrl}
              disabled={locked}
              onChange={e => patch({ brokerUrl: e.target.value })}
            />
            <span className="vol-help">Where your relay registers itself.</span>
          </div>

          <div className="vol-field">
            <label className="vol-label" htmlFor="hub-address">
              Hub address
            </label>
            <input
              id="hub-address"
              className="vol-input"
              type="text"
              placeholder="host:port"
              value={form.hubAddress}
              disabled={locked}
              onChange={e => patch({ hubAddress: e.target.value })}
            />
            <span className="vol-help">
              Relay hub for computers that can{'\u2019'}t accept incoming connections. Leave empty
              unless the project has published one.
            </span>
          </div>
        </>
      )}

      <div className="vol-save-row">
        <button
          type="button"
          className="vol-save"
          disabled={locked || busy}
          onClick={() => void save()}
        >
          Save
        </button>
        {saveStatus.kind === 'saved' && <span className="vol-save-status is-saved">Saved</span>}
        {saveStatus.kind === 'error' && (
          <span className="vol-save-status is-error">{saveStatus.message}</span>
        )}
      </div>
    </div>
  );
}
