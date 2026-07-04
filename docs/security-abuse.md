# Security And Abuse Notes

## Current Risk

Direct volunteer exits are simple, but they put volunteers in the destination-visible path. Destination services, abuse desks, and network operators may see the volunteer's IP as the source.

Before any public rollout, add controls for:

- Per-volunteer bandwidth and session limits.
- Destination blocklists for obvious abuse categories.
- Emergency relay disablement.
- Volunteer opt-in terms and warnings.
- Broker-side registration authentication.
- Health and reachability verification.
- Client and relay version enforcement.

## Broker Hardening

The broker should eventually use:

- Persistent storage.
- Signed relay descriptors.
- Per-client ephemeral VLESS credentials instead of one shared client ID per volunteer process.
- Volunteer authentication and reputation.
- Rate limiting.
- Audit logs for control-plane events.
- Separate public client API and private volunteer API.

The broker should avoid storing:

- Browsing destinations.
- Client traffic metadata.
- Full client IP logs unless operationally required.

## Relay Hardening

The volunteer CLI should eventually support:

- OS-level firewall policy generation.
- Bandwidth ceilings.
- Session ceilings.
- Automatic updates.
- Clear exit mode display.
- Dedicated exit-server forwarding mode.

## Dedicated Exit Mode

Dedicated exit mode should become the preferred public mode:

```text
Censored client -> volunteer entry relay -> dedicated exit server -> public website/app
```

This reduces volunteer exposure and enables centralized abuse handling at the dedicated exit layer.
