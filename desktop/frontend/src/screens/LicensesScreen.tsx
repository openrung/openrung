// Open-source licenses screen (desktop), ported from the RN LicensesScreen:
// GPL intro paragraph, "Source code" panel (opens the public repository),
// "Full license texts" panel (pushes the full-text screen), then the
// "Components" header and the per-component license panels from the bundled
// notices. Reached from About → Open-source licenses.
import { SettingRow } from '../components/SettingRow';
import { AppConfig } from '../core/config';
import { openExternal } from '../native/openExternal';
import { components } from '../licenses/notices';

interface Props {
  onBack: () => void;
  onOpenFullText: () => void;
}

export function LicensesScreen({ onBack, onOpenFullText }: Props) {
  return (
    <div className="or-screen">
      <div className="or-screen-header">
        <button type="button" className="or-back-btn" onClick={onBack} aria-label="Back">
          ←
        </button>
        <h1 className="or-screen-title">Open-source licenses</h1>
      </div>

      <p className="or-licenses-intro">
        OpenRung is free software licensed under GPL-3.0-or-later because it is built from
        GPL-licensed code and bundles the sing-box engine. The complete corresponding source for
        this build is available at the link below.
      </p>

      <SettingRow
        title="Source code"
        subtitle={AppConfig.SOURCE_URL}
        onPress={() => openExternal(AppConfig.SOURCE_URL)}
      />

      <SettingRow
        title="Full license texts"
        subtitle="GNU GPL-3.0 and third-party notices."
        onPress={onOpenFullText}
      />

      <span className="or-section-header">COMPONENTS</span>
      {components.map(entry => (
        <SettingRow key={entry.name} title={entry.name} subtitle={entry.license} />
      ))}
    </div>
  );
}
