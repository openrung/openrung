// @vitest-environment node
import { describe, expect, it } from 'vitest';

import { rewriteAssetRequires } from './assetRequires';

describe('rewriteAssetRequires', () => {
  it('turns image requires into URL imports', () => {
    const source = [
      "import React from 'react';",
      "const QUOTE = require('../assets/pull-quote.png');",
      'const PHOTO = require("./photo.jpeg");',
    ].join('\n');
    expect(rewriteAssetRequires(source)).toBe(
      [
        'import __mobileAsset0 from "../assets/pull-quote.png";',
        'import __mobileAsset1 from "./photo.jpeg";',
        "import React from 'react';",
        'const QUOTE = __mobileAsset0;',
        'const PHOTO = __mobileAsset1;',
      ].join('\n'),
    );
  });

  it('leaves code without image requires alone', () => {
    expect(rewriteAssetRequires("const fs = require('node:fs');")).toBeNull();
    expect(rewriteAssetRequires("import x from './x';")).toBeNull();
  });
});
