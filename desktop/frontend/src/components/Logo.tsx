// The OpenRung mark — a white ladder on the brand-green rounded square. Inlined
// as SVG (not an imported asset) so it renders with zero network, matching the
// app's fully-offline, bundled-assets posture. Kept in sync with
// docs/openrung-mark.svg.
export function Logo({ size = 44 }: { size?: number }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 256 256"
      xmlns="http://www.w3.org/2000/svg"
      role="img"
      aria-label="OpenRung"
    >
      <rect width="256" height="256" rx="58" fill="#1d8a4f" />
      <g fill="#ffffff" transform="rotate(18 128 128)">
        <rect x="101" y="52" width="13" height="38" rx="3" />
        <rect x="101" y="94" width="13" height="47" rx="3" />
        <rect x="101" y="145" width="13" height="59" rx="3" />
        <rect x="142" y="52" width="13" height="38" rx="3" />
        <rect x="142" y="94" width="13" height="47" rx="3" />
        <rect x="142" y="145" width="13" height="59" rx="3" />
        <rect x="101" y="60" width="54" height="13" rx="3" />
        <rect x="101" y="111" width="54" height="13" rx="3" />
        <rect x="101" y="162" width="54" height="13" rx="3" />
      </g>
    </svg>
  );
}
