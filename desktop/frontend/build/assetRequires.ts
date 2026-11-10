/**
 * Metro turns `require('./image.png')` into an asset id; the browser has no
 * `require`. Rewrites those calls into URL imports, which react-native-web's
 * Image accepts as a source. Returns null when the code has none.
 */
export function rewriteAssetRequires(code: string): string | null {
  const assetRequire = /require\((['"])([^'"]+\.(?:png|jpe?g|gif|webp))\1\)/g;
  const imports: string[] = [];
  const rewritten = code.replace(assetRequire, (_match, _quote, specifier: string) => {
    const name = `__mobileAsset${imports.length}`;
    imports.push(`import ${name} from ${JSON.stringify(specifier)};`);
    return name;
  });
  return imports.length === 0 ? null : `${imports.join('\n')}\n${rewritten}`;
}
