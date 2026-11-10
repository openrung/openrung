// Entry point: mounts the mobile app's root component, unmodified, through
// react-native-web.
import 'maplibre-gl/dist/maplibre-gl.css';

import { AppRegistry } from 'react-native-web';

import { installNativeModules } from './native/installNativeModules';

// Must run before the mobile modules evaluate: their native wrappers read
// NativeModules at import time and fall back to mocks when a module is absent.
installNativeModules();

const { default: App } = await import('@mobile/App');

AppRegistry.registerComponent('OpenRung', () => App);
AppRegistry.runApplication('OpenRung', { rootTag: document.getElementById('root') });
