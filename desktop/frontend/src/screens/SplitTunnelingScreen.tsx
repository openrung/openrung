import { useEffect, useRef } from 'react';
import { SettingRow } from '../components/SettingRow';
import { TerminalSwitch } from '../components/TerminalSwitch';
import {
  hydrateSplitTunnel,
  setSplitTunnel,
  useAppState,
} from '../state/store';

interface Props {
  onBack: () => void;
}

const COUNTRY_ORDER = ['ir', 'cn'] as const;
type CountryCode = (typeof COUNTRY_ORDER)[number];

/**
 * Preset split tunneling for traffic already using the desktop local proxy.
 * Per-app exclusions are intentionally absent: choosing which desktop apps
 * use the proxy is already the outer, app-level split.
 */
export function SplitTunnelingScreen({ onBack }: Props) {
  const splitTunnel = useAppState().splitTunnel;
  const titleRef = useRef<HTMLHeadingElement>(null);

  useEffect(() => {
    // Defensive for isolated renders; App also hydrates this at launch so the
    // Settings summary never misreports the active routing policy.
    hydrateSplitTunnel();
    // Treat the pushed screen as a route change for keyboard and screen-reader
    // users instead of leaving focus on the button that was just unmounted.
    titleRef.current?.focus();
  }, []);

  const toggleCountry = (code: CountryCode, on: boolean) => {
    const bypassCountries = COUNTRY_ORDER.filter(preset =>
      preset === code ? on : splitTunnel.bypassCountries.includes(preset),
    );
    setSplitTunnel({ bypassCountries });
  };

  return (
    <div className="or-screen or-split-screen">
      <div className="or-screen-header">
        <button type="button" className="or-back-btn" onClick={onBack} aria-label="Back">
          ←
        </button>
        <h1 ref={titleRef} className="or-screen-title" tabIndex={-1}>
          Split tunneling
        </h1>
      </div>

      <SettingRow
        title="Split tunneling"
        subtitle="Route selected proxy traffic directly instead of through the relay."
        trailing={
          <TerminalSwitch
            label="Split tunneling"
            value={splitTunnel.enabled}
            onChange={enabled => setSplitTunnel({ enabled })}
          />
        }
      />

      <span className="or-section-header">BYPASS</span>
      <div
        className={`or-split-presets ${splitTunnel.enabled ? '' : 'is-disabled'}`}
        aria-disabled={!splitTunnel.enabled}
      >
        <SettingRow
          title="Local network"
          subtitle="Reach printers, TVs and other LAN devices directly."
          trailing={
            <TerminalSwitch
              label="Bypass local network"
              value={splitTunnel.bypassLan}
              disabled={!splitTunnel.enabled}
              onChange={bypassLan => setSplitTunnel({ bypassLan })}
            />
          }
        />
        <SettingRow
          title="Iranian sites & services"
          subtitle="Route Iranian services directly, at full speed."
          trailing={
            <TerminalSwitch
              label="Bypass Iranian sites and services"
              value={splitTunnel.bypassCountries.includes('ir')}
              disabled={!splitTunnel.enabled}
              onChange={on => toggleCountry('ir', on)}
            />
          }
        />
        <SettingRow
          title="Chinese sites & services"
          subtitle="Route Chinese services directly, at full speed."
          trailing={
            <TerminalSwitch
              label="Bypass Chinese sites and services"
              value={splitTunnel.bypassCountries.includes('cn')}
              disabled={!splitTunnel.enabled}
              onChange={on => toggleCountry('cn', on)}
            />
          }
        />
      </div>

      <p className="or-split-footer">
        Changes apply immediately; an active proxy connection reconnects for a few seconds. Apps
        that do not use the OpenRung proxy already connect directly.
      </p>
    </div>
  );
}
