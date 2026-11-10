// Stand-in for Metro's asset registry, which react-native-svg's web build
// imports to resolve numeric asset ids. Nothing here produces numeric ids
// (image requires become URL imports, see vite.config.ts), so lookups miss.
export function getAssetByID(): undefined {
  return undefined;
}
