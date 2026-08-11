interface Props {
  value: boolean;
  onChange: (value: boolean) => void;
  disabled?: boolean;
  label: string;
}

/** Compact terminal-style ON/OFF control shared with split-tunnel settings. */
export function TerminalSwitch({ value, onChange, disabled = false, label }: Props) {
  return (
    <button
      type="button"
      className={`or-terminal-switch ${value ? 'is-on' : ''}`}
      role="switch"
      aria-checked={value}
      aria-label={label}
      disabled={disabled}
      onClick={() => onChange(!value)}
    >
      {value ? 'ON' : 'OFF'}
    </button>
  );
}
