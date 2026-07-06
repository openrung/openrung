import { useEffect, useState } from 'react';
import { ExitNodeMap } from './components/ExitNodeMap';
import { ConnectCard } from './components/ConnectCard';
import { ConsolePanel } from './components/ConsolePanel';
import { RelayList } from './components/RelayList';
import { RecentsSection } from './components/RecentsSection';
import { ViewModeToggle, type ViewMode } from './components/ViewModeToggle';
import { HomeIcon, SlidersIcon, InfoIcon } from './components/NavIcons';
import { SettingsScreen } from './screens/SettingsScreen';
import { AboutScreen } from './screens/AboutScreen';
import { Logo } from './components/Logo';
import { useVpnState } from './state/useVpnState';
import { refreshDirectory } from './state/store';
import { isMock } from './native/OpenRungVpn';

type Tab = 'home' | 'settings' | 'about';

const NAV = [
  { key: 'home' as const, label: 'Home', Icon: HomeIcon },
  { key: 'settings' as const, label: 'Settings', Icon: SlidersIcon },
  { key: 'about' as const, label: 'About Us', Icon: InfoIcon },
];

export default function App() {
  const { state, prepareAndConnect } = useVpnState();
  const [tab, setTab] = useState<Tab>('home');
  const [viewMode, setViewMode] = useState<ViewMode>('map');
  const [consoleOpen, setConsoleOpen] = useState(false);

  // Populate the exit-node map once on mount (Go owns failover/429; the throttle
  // there caps broker hits regardless of how often this is called).
  useEffect(() => {
    void refreshDirectory();
  }, []);

  const regions = state.availableRegions;
  const connectTo = (code: string) => void prepareAndConnect(code);

  const chipLabel =
    state.directoryStatus === 'loaded'
      ? `${regions.length} locations available`
      : state.directoryStatus === 'failed'
        ? 'directory unavailable'
        : 'loading locations…';

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
        {tab === 'home' && (
          <>
            {/* base layer: the map is always mounted, full-bleed, so it stays
                the backdrop even in list mode (like Android). */}
            <ExitNodeMap regions={regions} onSelect={connectTo} />

            <div className="or-edge-fade" aria-hidden />

            {/* overlay column: header, toggle, list-or-spacer, bottom stack */}
            <div className="or-overlay">
              <header className="or-header">
                <div className="or-header-left">
                  <div className="or-wordmark-row">
                    <span className="or-wordmark">OpenRung</span>
                    <span className="or-cursor">▍</span>
                  </div>
                  <span className="or-tagline">volunteer relay network</span>
                </div>
                <div className="or-header-right">
                  <div className={`or-map-chip ${state.directoryStatus === 'failed' ? 'is-failed' : ''}`}>
                    {chipLabel}
                  </div>
                  {isMock && <span className="mock-badge">mock</span>}
                </div>
              </header>

              <div className="or-toggle-wrap">
                <ViewModeToggle mode={viewMode} onChange={setViewMode} />
              </div>

              {viewMode === 'list' ? (
                <RelayList
                  regions={regions}
                  status={state.directoryStatus}
                  onSelect={connectTo}
                  onRetry={() => void refreshDirectory(true)}
                />
              ) : (
                <div className="or-spacer" />
              )}

              <div className="or-bottom-stack">
                <RecentsSection recents={state.native.recents} onSelect={connectTo} />
                <ConnectCard />
              </div>
            </div>
          </>
        )}

        {tab === 'settings' && (
          <SettingsScreen consoleOpen={consoleOpen} onToggleConsole={() => setConsoleOpen(o => !o)} />
        )}

        {tab === 'about' && <AboutScreen />}

        {/* Console dock floats over any tab; toggled from Settings → Debug. */}
        {consoleOpen && (
          <div className="console-dock">
            <ConsolePanel />
          </div>
        )}
      </main>
    </div>
  );
}
