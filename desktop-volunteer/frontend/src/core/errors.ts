// Wails rejects Go errors as plain strings; the mock throws Error objects.
// Normalizes both into a display string.
export function errorMessage(err: unknown): string {
  if (err instanceof Error) {
    return err.message;
  }
  return String(err);
}
