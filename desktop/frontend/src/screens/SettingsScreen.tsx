// Settings tab (desktop), mirroring the RN SettingsScreen layout: a large
// title then sectioned panels. The desktop's operational settings are a
// read-only CONNECTION summary and a DIAGNOSTICS section whose Debug row
// toggles the connection console. (Language + speed test are RN features not
// yet ported to desktop.)
import { SettingRow } from '../components/SettingRow';
import { AppConfig } from '../core/config';

interface Props {
  consoleOpen: boolean;
  onToggleConsole: () => void;
}

export function SettingsScreen({ consoleOpen, onToggleConsole }: Props) {
  return (
    <div className="or-screen">
      <h1 className="or-screen-title">Settings</h1>

      <span className="or-section-header">CONNECTION</span>
      <SettingRow title="Broker" subtitle={AppConfig.DEFAULT_BROKER_URL} />
      <SettingRow title="Tunnel mode" subtitle="System proxy (per-app, no admin)" />

      <span className="or-section-header">DIAGNOSTICS</span>
      <SettingRow
        title="Debug"
        subtitle="Connection console and diagnostics."
        trailing={
          <button type="button" className="or-setting-action" onClick={onToggleConsole}>
            {consoleOpen ? 'Hide' : 'Show'}
          </button>
        }
      />
    </div>
  );
}
