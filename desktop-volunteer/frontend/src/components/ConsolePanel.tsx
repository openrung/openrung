import { useEffect, useRef } from 'react';

// Live relay log, fed by the bridge's logLines ring buffer. Prop-driven (the
// single volunteerStateChanged subscription lives in App via
// useVolunteerState) and auto-scrolls to the newest line.
export function ConsolePanel({ lines }: { lines: string[] }) {
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
