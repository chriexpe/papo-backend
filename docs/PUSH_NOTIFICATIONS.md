# Papo background push architecture

Papo is self-hosted, but the official Android application is a single signed
application with one platform push identity. Background notification delivery
must preserve both facts.

## Architecture

```text
Papo backend A ----\
Papo backend B -----+----> push.papo.chat ----> FCM ----> Papo Android
Papo backend C ----/
```

Self-hosted backends **do not** receive Firebase credentials and do not receive
FCM registration tokens.

Android registers its FCM token only with the Papo-operated relay. For each
logged-in Papo backend/workspace, Android asks the relay to mint a distinct
delivery capability. It then gives that backend:

- an HTTPS delivery `address` under `push.papo.chat/v1/deliver/...`
- a high-entropy `auth_token` valid only for that capability

The backend authenticates to the relay by possession of this scoped capability:

```http
POST /v1/deliver/<capability-id>
Authorization: Bearer <capability-secret>
Content-Type: application/json
```

A backend therefore has no relay account and no global API key. Compromise of
one server exposes only the capabilities stored by that server, not the
official Firebase project and not other Papo installations.

## Constraints that must not be simplified away

1. A killed/backgrounded Android process cannot be assumed to retain the Papo
   WebSocket. Timely message notifications require a system/distributor wake
   path such as FCM or a future UnifiedPush distributor.
2. A permanent foreground service is not an acceptable substitute for ordinary
   message push. It is user-visible and should remain reserved for ongoing work
   such as active voice/video calls.
3. Firebase sender credentials belong to the official Android app's Firebase
   project. Arbitrary self-hosted Papo servers must never receive them.
4. Backends must not receive raw FCM tokens either. FCM token rotation stays
   between Android and the relay, so backend registrations remain stable.
5. Do not replace Firebase-per-server with a global relay API key per server.
   Each backend receives only a per-installation capability delegated by the
   Android client.
6. A different capability must be minted for every backend/workspace, even when
   several servers target the same physical phone.
7. `DispatchMessageNotifications` remains the single notification-policy
   authority. Push consumes its delivery results; it must not recalculate
   mentions, replies, channel settings, permissions, or @everyone eligibility.
8. Client-provided destinations must not become SSRF. The current
   `papo_relay` provider accepts only HTTPS capability URLs on
   `push.papo.chat` under `/v1/deliver/` and does not follow redirects.
9. Relay delivery is best-effort. Failure must never fail message creation or
   the existing WebSocket delivery.
10. Without later payload encryption, the relay can see the notification
    payload it forwards. Do not describe this design as end-to-end encrypted.

## Session lifecycle

Push registrations belong to a Papo authentication connection, not only a
user. Normal refresh moves registrations to the replacement connection.
Logout/drop/reuse revocation removes them. This prevents an honest backend from
continuing to send to a session after it has been revoked.

## Privacy and routing

The installation UUID registered with a Papo backend is generated per
backend/workspace. It must not be the relay's global installation identifier,
which would allow unrelated self-hosted servers to correlate the same device.

The relay capability/route id is enough for Android to map a received push to
its locally stored workspace. The backend does not invent a global Papo server
identifier.
