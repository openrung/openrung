// Sidebar nav icons, ported from openrung-mobile-app/src/components/Icons.tsx
// (the tab-bar set: Home / Settings / About Us). Same 24x24 viewBox and
// currentColor stroke so they inherit the active/inactive tint.
interface IconProps {
  size?: number;
}

/** House outline (Home tab). */
export function HomeIcon({ size = 22 }: IconProps) {
  return (
    <svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round">
      <path d="M4 10.5 12 4l8 6.5V19a1.5 1.5 0 0 1-1.5 1.5h-13A1.5 1.5 0 0 1 4 19v-8.5Z" />
      <path d="M9.5 20.5v-6h5v6" />
    </svg>
  );
}

/** Three tuning sliders (Settings tab). */
export function SlidersIcon({ size = 22 }: IconProps) {
  return (
    <svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round">
      <line x1="4" y1="7" x2="20" y2="7" />
      <line x1="4" y1="12" x2="20" y2="12" />
      <line x1="4" y1="17" x2="20" y2="17" />
      <circle cx="9" cy="7" r="2.1" fill="none" />
      <circle cx="15" cy="12" r="2.1" fill="none" />
      <circle cx="7" cy="17" r="2.1" fill="none" />
    </svg>
  );
}

/** Info circle (About tab). */
export function InfoIcon({ size = 22 }: IconProps) {
  return (
    <svg width={size} height={size} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round">
      <circle cx="12" cy="12" r="8.5" />
      <line x1="12" y1="11" x2="12" y2="16" />
      <circle cx="12" cy="7.8" r="1.15" fill="currentColor" stroke="none" />
    </svg>
  );
}
