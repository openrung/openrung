import { useEffect, useRef } from 'react';
import { useVpnState } from '../state/useVpnState';

// Live tunnel log, fed by the bridge's logLines (the 80-line ring the Go
// service maintains). Auto-scrolls to the newest line.
export function ConsolePanel() {
  const { state } = useVpnState();
  const lines = state.native.logLines;
  const endRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    endRef.current?.scrollIntoView({ block: 'end' });
  }, [lines]);

  return (
    <div className="console-panel" data-testid="console-panel">
      {lines.length === 0 ? (
        <div className="console-empty">no activity yet</div>
      ) : (
        lines.map((line, i) => (
          <div
            key={i}
            className={line.includes('fail') || line.includes('error') ? 'console-line error' : 'console-line'}
          >
            {line}
          </div>
        ))
      )}
      <div ref={endRef} />
    </div>
  );
}
