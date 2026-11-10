// Stand-in for react-native-bottom-tabs (the iOS system tab bar). The mobile
// app only renders it when Platform.OS === 'ios'; on web it uses its own JS
// TabBar, so this exists purely to satisfy the import.
export interface AppleIcon {
  sfSymbol: string;
}

export default function TabView(): null {
  return null;
}
