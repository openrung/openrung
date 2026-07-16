import { describe, expect, it } from 'vitest';
import wailsConfig from '../../../wails.json';
import { APP_VERSION } from './config';

describe('APP_VERSION', () => {
  it('comes from the Wails product version', () => {
    expect(APP_VERSION).toBe(wailsConfig.info.productVersion);
  });
});
