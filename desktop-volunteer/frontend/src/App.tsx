import { useState } from 'react';
import { HomeIcon, SlidersIcon, InfoIcon } from './components/NavIcons';
import { Logo } from './components/Logo';
import { AboutScreen } from './screens/AboutScreen';
import { ConsentGate } from './screens/ConsentGate';
import { HomeScreen } from './screens/HomeScreen';
import { SettingsScreen } from './screens/SettingsScreen';
import { isConsentGateVisible } from './screens/consent';
import { useVolunteerState } from './state/useVolunteerState';

type Tab = 'home' | 'settings' | 'about';

const NAV = [
  { key: 'home' as const, label: 'Home', Icon: HomeIcon },
  { key: 'settings' as const, label: 'Settings', Icon: SlidersIcon },
  { key: 'about' as const, label: 'About', Icon: InfoIcon },
];

export default function App() {
  const { state, start, stop, acceptConsent } = useVolunteerState();
  const [tab, setTab] = useState<Tab>('home');

  // Hold a blank frame until the first bridge snapshot lands so the consent
  // gate never flashes for volunteers who already accepted.
  if (!state.hydrated) {
    return <div className="vol-boot" />;
  }

  if (isConsentGateVisible(state.hydrated, state.volunteer.consentAccepted)) {
    return (
      <ConsentGate consentAccepted={state.volunteer.consentAccepted} onAccept={acceptConsent} />
    );
  }

  return (
    <div className="app">
      <nav className="sidebar">
        <div className="wordmark">
          <Logo size={44} />
        </div>
        {NAV.map(({ key, label, Icon }) => (
          <button
            key={key}
            className={`nav-tab ${tab === key ? 'active' : ''}`}
            onClick={() => setTab(key)}
          >
            <Icon size={22} />
            <span className="nav-tab-label">{label}</span>
          </button>
        ))}
      </nav>

      <main className="main">
        {tab === 'home' && <HomeScreen state={state.volunteer} onStart={start} onStop={stop} />}
        {tab === 'settings' && <SettingsScreen state={state.volunteer} />}
        {tab === 'about' && <AboutScreen />}
      </main>
    </div>
  );
}
