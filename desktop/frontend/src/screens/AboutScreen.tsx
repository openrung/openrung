// About Us tab (desktop), ported from the RN AboutScreen: wordmark hero with a
// version pill, mission paragraph, a numbered "how it works" walkthrough, then
// source/licenses panels and the GPL footnote. Copy is verbatim from the RN
// app's en.ts strings.
import { SettingRow } from '../components/SettingRow';
import { AppConfig, APP_VERSION } from '../core/config';
import { openExternal } from '../native/openExternal';

const HOW_STEPS = [
  {
    title: 'Volunteers share bandwidth',
    body: 'People everywhere run small relay nodes on their own connections and register them with the network.',
  },
  {
    title: 'The broker finds your relay',
    body: 'When you connect, the broker hands your device a short list of healthy relays and the app picks the first one that answers.',
  },
  {
    title: 'Traffic rides an encrypted tunnel',
    body: 'Everything flows through a VLESS/REALITY tunnel that looks like ordinary TLS, and the VPN is fail-closed: no relay, no traffic.',
  },
];

interface Props {
  onOpenLicenses: () => void;
}

export function AboutScreen({ onOpenLicenses }: Props) {
  return (
    <div className="or-screen">
      <h1 className="or-screen-title">About us</h1>

      <div className="or-about-hero">
        <div className="or-about-hero-row">
          <span className="or-about-wordmark">OpenRung</span>
          <span className="or-about-version">v{APP_VERSION}</span>
        </div>
        <span className="or-tagline">volunteer relay network</span>
        <p className="or-about-mission">
          OpenRung routes your traffic through relays run by volunteers around the world, keeping the
          open internet reachable when networks are filtered. No accounts, no ads, no tracking — just
          people sharing bandwidth.
        </p>
      </div>

      <span className="or-section-header">HOW IT WORKS</span>
      <div className="or-about-steps">
        {HOW_STEPS.map((step, i) => (
          <div key={i} className="or-about-step">
            <span className="or-about-step-index">{String(i + 1).padStart(2, '0')}</span>
            <div className="or-about-step-text">
              <span className="or-about-step-title">{step.title}</span>
              <span className="or-about-step-body">{step.body}</span>
            </div>
          </div>
        ))}
      </div>

      <span className="or-section-header">PROJECT</span>
      <SettingRow
        title="Source code"
        subtitle={AppConfig.SOURCE_URL}
        onPress={() => openExternal(AppConfig.SOURCE_URL)}
      />
      <SettingRow
        title="Open-source licenses"
        subtitle="Licenses and attribution for bundled software."
        onPress={onOpenLicenses}
      />

      <p className="or-about-footnote">
        OpenRung is free software (GPL-3.0-or-later). Built by volunteers, for everyone.
      </p>
    </div>
  );
}
