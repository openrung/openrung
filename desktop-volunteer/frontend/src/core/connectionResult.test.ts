import { describe, expect, it } from 'vitest';
import { connectionResult } from './connectionResult';

describe('connectionResult', () => {
  it('shows the selected host and port for a direct relay', () => {
    expect(
      connectionResult({ transport: 'direct', publicEndpoint: '[2001:db8::25]:443' }),
    ).toBe('Direct on [2001:db8::25]:443');
  });

  it('identifies RelayHub without presenting its endpoint as local', () => {
    expect(
      connectionResult({ transport: 'tunnel', publicEndpoint: 'hub.example:9443' }),
    ).toBe('Via RelayHub');
  });

  it('does not claim a result until a route has been selected', () => {
    expect(connectionResult({ transport: '', publicEndpoint: '' })).toBeNull();
  });
});
