// About tab: what the app is, the licensing story (GPL app bundling MPL
// Xray-core), and external links opened in the user's real browser.
import { Logo } from '../components/Logo';
import { SettingRow } from '../components/SettingRow';
import { AppConfig, APP_VERSION } from '../core/config';
import { openExternal } from '../native/openExternal';

export function AboutScreen() {
  return (
    <div className="or-screen">
      <h1 className="or-screen-title">About</h1>

      <div className="or-about-hero">
        <div className="or-about-hero-row">
          <Logo size={34} />
          <span className="or-about-wordmark">OpenRung Volunteer</span>
          <span className="or-about-version">v{APP_VERSION}</span>
        </div>
        <span className="or-tagline">share your connection</span>
        <p className="or-about-mission">
          OpenRung Volunteer turns your computer into a relay for the OpenRung
          censorship-circumvention network. While it runs, people in censored countries can route
          their traffic through your internet connection to reach the open web. You choose when it
          runs and how much capacity to offer {'\u2014'} no accounts, no ads, no tracking.
        </p>
      </div>

      <span className="or-section-header">LICENSING</span>
      <div className="or-setting-row">
        <div className="or-setting-text">
          <span className="or-setting-title">Free software</span>
          <span className="or-setting-subtitle is-wrap">
            OpenRung Volunteer is free software (GPL-3.0-or-later). It bundles Xray-core (MPL-2.0)
            {' \u2014 '}source: github.com/XTLS/Xray-core.
          </span>
        </div>
      </div>

      <span className="or-section-header">LINKS</span>
      <SettingRow
        title="Website"
        subtitle="openrung.org"
        onPress={() => openExternal(AppConfig.WEBSITE_URL)}
      />
      <SettingRow
        title="Source code"
        subtitle="github.com/openrung/openrung"
        onPress={() => openExternal(AppConfig.SOURCE_URL)}
      />
      <SettingRow
        title="Xray-core source"
        subtitle="github.com/XTLS/Xray-core"
        onPress={() => openExternal(AppConfig.XRAY_SOURCE_URL)}
      />

      <p className="or-about-footnote">
        OpenRung is free software (GPL-3.0-or-later). Built by volunteers, for everyone.
      </p>
    </div>
  );
}
