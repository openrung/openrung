// A bordered settings/info row (title + subtitle, optional trailing control or
// click action), mirroring the RN SettingPanel. Renders as a button when
// onPress is given, otherwise a static panel.
import type { ReactNode } from 'react';

interface Props {
  title: string;
  subtitle?: string;
  trailing?: ReactNode;
  onPress?: () => void;
}

export function SettingRow({ title, subtitle, trailing, onPress }: Props) {
  const inner = (
    <>
      <div className="or-setting-text">
        <span className="or-setting-title">{title}</span>
        {subtitle != null && <span className="or-setting-subtitle">{subtitle}</span>}
      </div>
      {trailing}
      {onPress != null && trailing == null && <span className="or-setting-chevron">▸</span>}
    </>
  );

  if (onPress != null) {
    return (
      <button type="button" className="or-setting-row is-pressable" onClick={onPress}>
        {inner}
      </button>
    );
  }
  return <div className="or-setting-row">{inner}</div>;
}
