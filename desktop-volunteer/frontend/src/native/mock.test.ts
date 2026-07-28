import { describe, expect, it } from 'vitest';
import { MockVolunteerService } from './mock';

describe('MockVolunteerService direct setup', () => {
  it('is explicit, idempotent, and reversible without touching the OS', async () => {
    const service = new MockVolunteerService();

    expect((await service.getDirectSetupStatus()).state).toBe('needs_setup');

    const enabled = await service.enableDirectConnections();
    expect(enabled.state).toBe('ready');
    expect(enabled.canRemove).toBe(true);
    expect(await service.enableDirectConnections()).toEqual(enabled);
    expect((await service.getState()).directSetup).toEqual(enabled);

    const removed = await service.removeDirectConnections();
    expect(removed.state).toBe('needs_setup');
    expect(removed.canEnable).toBe(true);
  });
});
