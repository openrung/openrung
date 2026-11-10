// What the mobile source and its libraries get for `import ... from
// 'react-native'`: react-native-web, plus the few React Native exports that
// react-native-web does not provide but library code still names.
export * from 'react-native-web';

/**
 * Native TurboModules do not exist in a webview. react-native-svg's web build
 * still references one lazily (touch sounds on Android); the lookup must
 * resolve at bundle time and is never reached at runtime.
 */
export const TurboModuleRegistry = {
  get(): null {
    return null;
  },
  getEnforcing(name: string): never {
    throw new Error(`TurboModule '${name}' is not available on web`);
  },
};
