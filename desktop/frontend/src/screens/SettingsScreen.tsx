// Settings tab (desktop), mirroring the RN SettingsScreen layout: a large
// title then sectioned panels. The desktop's operational settings are a
// read-only CONNECTION summary and a DIAGNOSTICS section whose Debug row
// toggles the connection console. (Language + speed test are RN features not
// yet ported to desktop.)
import { useEffect, useState } from 'react';
import { SettingRow } from '../components/SettingRow';
import { AppConfig } from '../core/config';
import { copyText, OpenRungVpn } from '../native/OpenRungVpn';
import type { ConnectionStatus, NativeProxyInfo } from '../native/types';

interface Props {
  consoleOpen: boolean;
  connectionStatus: ConnectionStatus;
  onToggleConsole: () => void;
}

export function SettingsScreen({ consoleOpen, connectionStatus, onToggleConsole }: Props) {
  const [proxyInfo, setProxyInfo] = useState<NativeProxyInfo | null>(null);
  const [proxyError, setProxyError] = useState<string | null>(null);
  const [copied, setCopied] = useState<'endpoint' | 'enable' | 'disable' | null>(null);
  const connected = connectionStatus === 'connected';

  useEffect(() => {
    let active = true;
    void OpenRungVpn.getProxyInfo()
      .then(info => {
        if (active) setProxyInfo(info);
      })
      .catch(error => {
        if (active) setProxyError(String(error));
      });
    return () => {
      active = false;
    };
  }, []);

  const copy = async (kind: 'endpoint' | 'enable' | 'disable', value: string) => {
    try {
      await copyText(value);
    } catch {
      return;
    }
    setCopied(kind);
    window.setTimeout(() => setCopied(current => (current === kind ? null : current)), 1500);
  };

  return (
    <div className="or-screen">
      <h1 className="or-screen-title">Settings</h1>

      <span className="or-section-header">CONNECTION</span>
      <SettingRow title="Broker" subtitle={AppConfig.DEFAULT_BROKER_URL} />
      <SettingRow title="Tunnel mode" subtitle="System proxy (per-app, no admin)" />

      <span className="or-section-header">LOCAL PROXY</span>
      {proxyInfo != null ? (
        <>
          <SettingRow
            title="Endpoint"
            subtitle={`${proxyInfo.endpoint} · ${connected ? 'available now' : 'available while connected'}`}
            trailing={
              <button
                type="button"
                className="or-setting-action"
                onClick={() => void copy('endpoint', proxyInfo.endpoint)}
              >
                {copied === 'endpoint' ? 'Copied' : 'Copy'}
              </button>
            }
          />
          {proxyInfo.persistenceWarning != null && (
            <SettingRow title="Endpoint persistence" subtitle={proxyInfo.persistenceWarning} />
          )}
          {proxyInfo.shellIntegrationError != null ? (
            <SettingRow title="Shell integration" subtitle={proxyInfo.shellIntegrationError} />
          ) : proxyInfo.shellIntegration ? (
            <>
              <SettingRow
                title="Enable in this shell"
                subtitle={
                  connected
                    ? 'Preserves existing proxy values and unset state.'
                    : 'Connect OpenRung before enabling this shell.'
                }
                trailing={
                  <button
                    type="button"
                    className="or-setting-action"
                    disabled={!connected}
                    onClick={() => void copy('enable', proxyInfo.enableCommand)}
                  >
                    {copied === 'enable' ? 'Copied' : 'Copy'}
                  </button>
                }
              />
              <SettingRow
                title="Restore this shell"
                subtitle="Run after disconnect, failure, quit, or crash."
                trailing={
                  <button
                    type="button"
                    className="or-setting-action"
                    onClick={() => void copy('disable', proxyInfo.disableCommand)}
                  >
                    {copied === 'disable' ? 'Copied' : 'Copy'}
                  </button>
                }
              />
            </>
          ) : null}
        </>
      ) : (
        <SettingRow
          title="Endpoint"
          subtitle={proxyError ?? 'Preparing the stable local proxy endpoint…'}
        />
      )}

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
