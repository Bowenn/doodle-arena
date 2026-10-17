# ADR 0006: How the lobby and game servers authenticate each other over internal gRPC

- Status: Accepted (Option C for now; see "When to revisit")
- Date: 2026-10-16
- Deciders: Bowen

## Context

Task 2.1 added two internal gRPC services (design doc, section 6.1), both plaintext and unauthenticated:

- `doodle.lobby.v1.RegistryService.Heartbeat`, served by the lobby on port 9090. Every game server calls it every 2 seconds with its `server_id`, `ws_address`, `control_address`, rooms, socket count and draining flag.
- `doodle.game.v1.ControlService` (`AllocateRoom`, `DrainServer`), served by each game server on port 9090 and called by the lobby.

ADR 0001 keeps these ports off the gateway, so nothing outside the Docker network (or, in AWS, the VPC) can reach them. Inside it, any process can. Locally that is every container on the single Compose network: auth, assets and its Celery workers, ai, marketplace, notifier, Kafka and the data stores. A remote-code-execution bug in any of them (assets renders user-submitted drawings, a classic place for one) puts the attacker next to both gRPC ports.

### The threat today

What a process on the internal network can do:

- **Impersonate a real game server.** `Registry.Record` stores whatever arrives under the given `server_id`, so a heartbeat claiming `game-server-1` replaces the real entry. With a `control_address` of its own, the next `AllocateRoom` for that server goes to the attacker. The real server's next heartbeat (2 seconds later) puts it back, so the entry flaps.
- **Register a fake server.** A new `server_id` reporting 0 sockets is the least loaded, so new rooms go to it, and its `AllocateRoom` accepts them all. Players then get a URL for a gateway path that has no server, or a real server that doesn't have their room, so no new room can be played.
- **Hide a dead server.** Heartbeats under a dead server's id keep its entry and its rooms alive, so failover (task 2.5) never starts.
- **Drive any game server.** `AllocateRoom` can fill a server up to its room limit, and `DrainServer` can drain every server, which also stops room creation.

What it can't do, because of ADR 0005: let anyone into a room. Only the lobby holds the join-ticket private key, and a game server accepts a player only with a ticket for that room. The attack is on availability and on the lobby's view of the fleet, not on who plays in which room.

### Mitigations already in place (task 2.1)

- `ws_address` must be a gateway path under `/ws` (never a full URL), so a forged heartbeat can't send browsers to another host.
- `control_address` must be `host:port` with a DNS name or IPv4 address. It can still name any internal host, so the lobby can be made to open gRPC connections to, for example, `postgres:5432`. These fail at the HTTP/2 handshake, but they are a reachability probe.
- A heartbeat only refreshes rooms whose stored `server_id` matches the sender. This stops one server from keeping another server's rooms alive, but not a forger who simply uses the real server's id.
- SSRF from inside the system is unlikely to reach gRPC: the notifier refuses internal addresses (section 4.7), and an HTTP/1 webhook request can't speak gRPC's HTTP/2.

All of these limit the damage of a forged message. None of them tells the receiver who sent it.

### Requirements for any option

- **Bind identity to `server_id`.** Authenticating "some game server" isn't enough on its own: an authenticated `game-server-2` could still send heartbeats as `game-server-1`. Either the credential names the server and the lobby checks that it matches `server_id`, or we accept that game servers can impersonate each other (see "per-instance or per-class identity" below).
- **Both directions.** The lobby must authenticate game servers on `RegistryService`, and game servers must authenticate the lobby on `ControlService`.
- **No new dependency on the hot path.** A heartbeat every 2 seconds and every room creation must not wait on auth or any other service. Auth stays off the path (ADR 0001).
- **Rotation without a coordinated restart**, as for the JWT and ticket keys.
- **Bind `control_address` to the caller too** (found in the spike). An authenticated `game-server-2` could otherwise report `control_address: game-server-1:9090` under its own `server_id`, and the lobby would place `game-server-2`'s rooms on `game-server-1`. The host in `control_address` must equal the caller's authenticated name.
- **Check the caller's role, not only its name** (found in the spike). With one CA or one key bundle for every service, the lobby's own credential is valid at the registry. The registry accepts only `game-server` callers, and `ControlService` only the `lobby`.
- Both sides are Go services, so library support in other languages doesn't matter here (as in ADR 0005).

### Per-instance or per-class identity

Every game server already shares the `room:*` Redis prefix and the `round-logs` bucket, and nothing but the lease and fencing token (section 9) keeps one game server from writing another's room. A compromised game server is therefore already very powerful, whatever this ADR decides. Per-instance identity (one credential per `server_id`) stops a game server from impersonating its peers to the lobby. Per-class identity (one credential shared by every game server) stops everything else on the network, which is the threat above, and is simpler to issue when instances come and go (Fargate tasks in ADR 0003 Option B). The choice applies to Options A and B alike.

## Options

### Option A: mutual TLS with a private CA

Every service has an X.509 certificate signed by an internal CA. Each gRPC connection is TLS 1.3 in both directions: the lobby presents its certificate when it calls `ControlService`, and a game server presents its certificate when it calls `RegistryService`. Each side trusts only the internal CA and checks the peer's identity (a SAN such as `spiffe://doodle/game-server/game-server-1`, or a DNS SAN). The Heartbeat handler reads the peer certificate (`peer.FromContext`) and rejects a `server_id` that doesn't match it.

- Locally: `scripts/gen-internal-certs.sh`, in the style of `scripts/gen-jwt-key.sh`, creates a CA and one key and certificate per service under `secrets/` (gitignored), once. `make up` runs it and Compose mounts each service's files as secrets. In Go, `credentials.NewTLS` with `ClientAuth: tls.RequireAndVerifyClientCert` on both servers.
- In AWS: App Mesh, the usual answer for "a service mesh with mTLS on ECS", reached end of support on 2026-09-30. Its successor, ECS Service Connect, can encrypt traffic with certificates from AWS Private CA, but AWS lists mutual authentication for it as not currently supported (check before relying on this), so the application still has to do mTLS itself. Certificates then come from AWS Private CA (USD 400 a month per CA in general-purpose mode, or USD 50 a month in short-lived mode, whose certificates last at most 7 days and so need automated renewal), or from the same script's CA with its key in Secrets Manager (almost free, but we run the CA).

- Pros: authenticates both directions and encrypts the traffic in one mechanism, so nothing can be sniffed or replayed. Identity is checked once per connection, not per call. It is the industry-standard answer for service-to-service authentication and maps directly onto service meshes and SPIFFE, which is strong interview material. The certificate can name the server, so per-instance identity comes almost free locally.
- Cons: the most moving parts: a CA, certificate issuance, expiry and rotation (Go's `tls.Config.GetCertificate` can reload certificates, but someone has to replace them). Debugging is harder: `grpcurl` needs `-cacert`, `-cert` and `-key`, and a certificate mistake shows up as an opaque handshake failure. Per-instance certificates on Fargate need issuance at task start. In AWS it costs either a monthly Private CA fee or the work of running our own CA. Tests need certificates too (easy to generate in Go with `crypto/x509`).

### Option B: a signed service token on every call

Each caller signs a short-lived token and sends it as gRPC metadata (`authorization: Bearer …`) through `credentials.PerRPCCredentials`. A server interceptor verifies it. The token reuses the machinery of ADR 0005: a JWT with `alg: EdDSA`, `kid`, `iss` (the calling service), `sub` (the `server_id` for a game server), `aud` naming the callee (`doodle-lobby-registry` or `doodle-game-control`), and `exp` about 60 seconds ahead. Each service has its own Ed25519 identity key; receivers hold only the public keys, from a multi-key PEM file like `secrets/ticket-public.pem`. Tokens are self-issued, so auth isn't involved.

- Locally: `scripts/gen-service-keys.sh` (a copy of `gen-ticket-key.sh`) creates the lobby's key and the game servers' key or keys; Compose mounts each private key into its own service and the public keys into the receivers.
- In AWS: the private keys live in Secrets Manager, as ADR 0005 already plans for the ticket key. No AWS service is needed. To protect the token in transit, add one-way TLS: ECS Service Connect TLS, or server certificates only (no client certificates, so much less than Option A).

- Pros: the same pattern, library (`golang-jwt`) and tests as join tickets, so the code is familiar (an interceptor and a credentials type; the spike needed about 200 lines per service, see "Spike results") and easy to explain. Only public keys are distributed, and rotation is the same "publish the new public key first" as ADR 0005. The `sub` claim binds the caller to its `server_id`. Easy to debug: a rejected call says why (expired, wrong audience, unknown key). Nothing to pay in AWS.
- Cons: without TLS the token travels in plaintext, so anyone who can sniff the internal network can replay it until it expires (up to 60 seconds), and the traffic itself is readable and alterable. Go's gRPC refuses per-call credentials on an insecure connection unless they declare they don't need transport security, which is exactly this weakness. Making tokens single-use (as join tickets are) would cost a Redis write per heartbeat. A signature is computed and checked on every call (cheap with Ed25519, and it can be cached for a few seconds). It is a custom scheme that we must get right ourselves, rather than a standard one.

### Option C: network isolation only

Keep gRPC unauthenticated and restrict who can reach port 9090.

- Locally: today everything shares one Compose network. A separate `control` network for the lobby and game servers doesn't by itself isolate the port, because the lobby and game servers must also join the main network (for Redis, Kafka, the collector and the gateway) and their gRPC listeners bind to every interface. True isolation needs the listener bound to the control network's address, or a firewall rule, both awkward in Compose. So locally Option C is mostly "as now".
- In AWS: security groups that reference each other: the lobby's group accepts 9090 only from the game servers' group, and the game servers' group accepts 9090 only from the lobby's group. With ECS on Fargate (ADR 0003 Options B and C), every task has its own network interface and security group, so this is enforced per service. With EC2 hosts running the Compose stack (ADR 0003 Option A), every container on a host shares the host's security group, so security groups separate nothing between services on the same host.

- Pros: no code, no keys, no certificates, no added latency. Security groups are free, standard, and worth having with any option (defense in depth). It is honest about the actual risk for a student project with no real money that can leave the system.
- Cons: it is not authentication. Anything that gets onto an allowed host or task, such as a compromised game server or a misconfigured security group, is fully trusted, including to impersonate other game servers. It is the "trusted network" model that ADR 0001 rejected for user requests ("services don't trust the network"), so it is inconsistent with the project's zero-trust story and weak in an interview. It provides no isolation at all locally, and almost none under ADR 0003 Option A.

### What tips the choice

- If the interview story "zero trust between services" matters most, A, with B as the cheaper alternative to talk about.
- If local simplicity and reuse of ADR 0005 matter most, B, with one-way TLS added in AWS.
- If the time goes to failover and the marketplace instead, C, written down as a known gap with the attacks above.
- C's security groups apply in AWS whatever is chosen. The choice is what to add on top.

## When to decide

No task before 5.2 depends on this choice. None of the options changes `proto/`: certificates live in the transport, and tokens in gRPC metadata. Each one adds a layer around the existing calls (the listener and dial options in `services/lobby/cmd/lobby/main.go`, `services/lobby/internal/rooms/allocator.go` and `services/game-server/cmd/game-server/main.go`, plus one `server_id` check in the Heartbeat handler), so it costs the same now or after Stage 2.

- **2.2–2.4, 2.6, Stages 3 and 4, 5.1:** independent of it.
- **2.5 (failover):** can be built first. Failover trusts heartbeats, but locally nobody forges them. Adding authentication afterwards doesn't change the takeover logic.
- **5.2 (AWS deployment): the deadline.** Terraform needs the security groups (all options), and the secrets or the CA (A and B). This is also the first time the system is reachable by strangers.
- **5.3 (first measurements):** should run with the final transport, so the recorded numbers include its cost (small for all three).
- **6.1 (relays):** if relays register with the lobby, they use the same mechanism.

## Prior art

How similar systems authenticate the equivalent calls (checked 2026-10-16):

- **Agones** (game servers on Kubernetes): its allocator service, the closest match to our `AllocateRoom`, requires mTLS, and only client certificates on an allowlist may allocate. A game server talks to the SDK sidecar in its own pod over localhost, so its health pings never cross the network; Kubernetes itself knows which pods exist.
- **Amazon GameLift Servers** (Anywhere and container fleets): a game server process registers with the service using a short-lived auth token (`GetComputeAuthToken`) that must be refreshed before it expires. This is Option B with AWS as the token issuer.
- **Nakama** (Heroic Labs game backend): server-to-server RPCs use a static shared key (`runtime.http_key`) that bypasses session authentication. This is a weaker form of Option B: one long-lived shared secret, no expiry, and anyone holding it can call everything.
- **Service meshes** (Istio, Linkerd) and **SPIFFE/SPIRE**: automatic mTLS between every pair of services, with certificates that rotate every few hours. They use tokens only to bootstrap identity: Linkerd's proxy presents its Kubernetes service-account token to get its certificate.
- **Managed clouds:** Google Cloud Run services call each other with Google-signed ID tokens whose audience is the callee (Option B, managed issuer). AWS signs calls with IAM SigV4 (for example VPC Lattice auth policies), which also has the shape of Option B. VPC Lattice routes to a service, not to a chosen instance, so it doesn't fit `AllocateRoom`, which must reach one particular game server.
- **Small teams and early-stage systems** mostly run Option C: TLS at the edge, a private network behind it.

The common pattern in production is mTLS for transport identity and tokens for "on whose behalf". Our internal calls carry no end user, so either one alone is enough.

## Cost in AWS

Only what each option adds, in us-east-1 (prices checked 2026-10-16). The stack is one lobby and two game servers, with game servers authenticated per class (one credential for all of them). Two scenarios:

- **Informal and internal:** up about 40 hours a month for demos and measurements, torn down in between (ADR 0003's suggestion).
- **Public:** always on (730 hours a month), with 2 to 10 game servers.

| Option | Informal, internal | Public, always on | What the money buys |
|---|---|---|---|
| C: security groups only | USD 0 | USD 0 | Security groups are free |
| B: service tokens, own server certificates for TLS | under USD 0.20 | about USD 2, or 0 with Parameter Store | 4–5 secrets at USD 0.40 a month each, billed by the hour; SSM Parameter Store standard `SecureString` is free |
| A: mTLS, own CA (the local script, keys in Secrets Manager) | under USD 0.20 | about USD 1–5 | One certificate and key per service; more with per-instance identity |
| A: mTLS, AWS Private CA short-lived mode | about USD 4 (the first CA per Region is free for 30 days) | about USD 55 (2 game servers) to USD 70 (10) | USD 50 a month per CA, billed pro rata until it is deleted, plus USD 0.058 per certificate (one per task start and per renewal; certificates last at most 7 days) |
| A: mTLS, AWS Private CA general-purpose mode | about USD 22 | over USD 400 | USD 400 a month per CA plus USD 0.75 per certificate; no reason to pick it here |
| B with ECS Service Connect TLS for transport | about USD 4–5 | about USD 70–75 | A short-lived Private CA, plus the Envoy sidecar that Service Connect adds to every task (AWS suggests 0.25 vCPU and 64 MiB, about USD 6 a task per month on ARM Fargate) |

For scale: an always-on ARM Fargate task with 0.25 vCPU and 0.5 GB costs about USD 7 a month, so the roughly ten small service tasks cost about USD 70–90 a month before the load balancer (about USD 16 a month plus usage) and any NAT gateway (about USD 33 a month plus data). Options A with our own CA, B and C add under 3% to that. A Private CA adds 50% or more. Service Connect also load-balances across a service's tasks rather than addressing one, so each game server would have to be its own ECS service.

Going public changes the risk much more than the cost. All the costs above scale with the number of services and tasks, not with players. But a public stack accepts uploads and requests from strangers, so a compromised container stops being hypothetical.

## Interactions with other ADRs

- **ADR 0001 (JWT verification):** user access tokens must never authenticate service calls, and service credentials must never be accepted on public APIs. ADR 0001 already says service-to-service tokens get their own audiences. Option B follows that: separate keys (never auth's RS256 key), and audiences that no public endpoint accepts (public endpoints require `aud == doodle-api`). ADR 0001 also put auth's `/internal/api-keys/verify` on an internal port that relies on network isolation alone. Whatever this ADR picks becomes the pattern for that endpoint later, but it is out of scope here.
- **ADR 0005 (join tickets):** join tickets stay, whatever is chosen: browsers can't do mTLS, and tickets carry the lobby's decision about a player and a room. The join-ticket key is for tickets only. Under Option B the lobby gets a separate identity key, so a token for one purpose can never verify for the other, even if an `aud` check is missed. Option A or B would also give the game servers an authenticated channel to the lobby, which could later replace the mounted `ticket-public.pem` file (key distribution was left to task 2.1).
- **ADR 0003 (AWS shape, still Proposed):** Option C's strength depends on it (per-task security groups on Fargate, none between containers on one EC2 host). Option A's cost depends on it (Private CA fee, or our own CA in Secrets Manager; App Mesh is no longer available). Option B is the same everywhere.
- **ADR 0002 (WebSocket routing, still Proposed):** no direct interaction. `ws_address` stays a gateway path either way.

## Spike results (2026-10-16)

Both A and B were prototyped on local branches that are never merged: `spike/adr-0006-base` (shared bufconn tests, skipped), `spike/adr-0006-option-a-mtls` and `spike/adr-0006-option-b-tokens`. The same tests pass on both prototypes: no credential, an untrusted or expired one, `game-server-2` sending for `game-server-1`, and the lobby's credential at the registry are all rejected. `make up` and `make smoke` pass on both.

| | A: mTLS | B: service tokens |
|---|---|---|
| Production Go, both services | +400 lines, standard library only | +631 lines, adds `golang-jwt` to game-server |
| Call on an open connection (bufconn, M4) | 16 µs (15.5 without auth) | 54 µs |
| New connection plus call | 790 µs (214 without auth) | 300 µs |
| What a rejected caller sees | `Unavailable` with transport text: the TLS handshake fails before any interceptor runs | `Unauthenticated` with the reason (expired, wrong audience, unknown key) |
| `grpcurl` | `-cacert`, `-cert`, `-key` and `-authority` on every call | a freshly minted token every 60 seconds, so a `cmd/svctoken` tool |

What it taught us:

- **A is less code than expected, and its cost is operational:** the CA, expiry, rotation and debugging. A certificate mistake looks like a network outage. The lobby's allocator would try such a server last, as if it were dead, and logging failed handshakes needs a wrapper around the transport credentials, not an interceptor.
- **B is about twice the code expected**, because it rebuilds what TLS gives for free: loading keys, binding each key to one identity, caching tokens and checking claims. A plain multi-key file with a `kid` isn't enough: each public key must be bound to exactly one identity, or `game-server-2`'s key can sign `sub = game-server-1`.
- **B on plaintext authenticates the caller but not the message.** The token doesn't cover the request body, so anyone who can see and change traffic can keep the token and change the heartbeat. Containers have `NET_RAW` by default, so on one bridge network this isn't purely hypothetical. A also gets integrity, and it proves to each caller that it reached the right server.
- Both needed the two new requirements above: `control_address` bound to the caller, and a role check.

## Decision

Option C: no authentication on the internal gRPC for now. The lobby and game servers rely on the network boundary, the task 2.1 input checks, and, in AWS, security groups.

Why:

- **The real exposure is small today.** The system is a portfolio project with no known number of real users, and its coins are closed-loop: nothing of real value can leave it. The attacks above need code already running inside the network, and they cost availability, not room access or money. Join tickets (ADR 0005) already decide who plays in which room, and the marketplace never trusts these calls.
- **It costs nothing**, in code or in AWS, so the time goes to the parts the project is built to show: failover, the ledger and scale.
- **Changing later is cheap and already explored.** Neither A nor B touches `proto/`, and each adds a layer around four call sites plus a few checks in the handlers. Both are prototyped, with shared tests that only need their setup file switched (see "Spike results").
- **Rejected for now: A and B**, not because they're wrong, but because they buy protection against a threat that doesn't exist yet, at the price of keys or certificates in every environment. They stay the planned upgrade path (see "When to revisit").

## Consequences

- Locally nothing changes. Every container on the Compose network can still reach both gRPC ports, so the attacks in "The threat today" remain possible from inside. This is a known, written-down gap, not an oversight.
- The task 2.1 checks stay and must not be loosened: `ws_address` is a gateway path, `control_address` is `host:port`, and a heartbeat only refreshes rooms of the same `server_id`.
- **Task 5.2 must add the security groups:** the lobby accepts 9090 only from the game servers' group, and the game servers accept 9090 only from the lobby's group. Nothing else reaches either port.
- **Keep the seam narrow** so A or B can drop in. Transport credentials and interceptors are set only where the gRPC servers and clients are built: `services/lobby/cmd/lobby/main.go`, `services/lobby/internal/rooms/allocator.go` and `services/game-server/cmd/game-server/main.go`. No handler may assume a peer identity exists. The spike's `svcauth.ServerOptions` / dial-options shape is the model, and its shared tests can be revived from `spike/adr-0006-base`.
- Under C, telemetry is the only alarm. The lobby already logs each new registration with its addresses, so an unexpected `server_id` shows up in Loki. A heartbeat that changes the `control_address` of a known `server_id` (the impersonation case) isn't logged yet; a warning for it is a cheap addition to the seam task.
- Interview angle: knowing what a control protects against and choosing not to pay for it yet; when the decision flips; and what the spike measured (A's handshake cost versus B's per-call cost, and why tokens on plaintext don't protect the message).

## When to revisit

Switch to A or B (both prototyped; the spike results favour A for integrity and less code, B for clearer errors) when any of these becomes true:

- **Real users:** the stack is opened to the public beyond demos and friends, or real money enters the system.
- **ADR 0003 picks EC2 hosts running Compose** (its Option A). Security groups then separate nothing between containers on the same host, so C protects almost nothing in AWS.
- **More callers:** Stage 6 relays, or any other service, start calling the internal gRPC.
- **An incident or finding** shows a container on the internal network was, or could be, compromised.

### Cost of switching after the project is complete

Estimated 2026-10-16, from the spike. Writing the code is the small part: the agent wrote both prototypes in about 20 minutes. Most of the cost is review, AWS wiring, the rollout and measuring again.

| Work | A: mTLS | B: service tokens |
|---|---|---|
| Port the spike to the final code (3 call sites today, plus relays if they register) | about 730 lines to review | about 920 lines to review |
| Production gaps the spike skipped | certificate reload, a handshake-failure log and metric, and an allocator that doesn't treat a bad certificate as a dead server | a `cmd/svctoken` tool for `grpcurl`, and optional verified-token caching |
| AWS (Terraform) | our own CA: certificates in Secrets Manager. Private CA: certificates issued when a task starts, renewal every 7 days or less, plus IAM | keys in Secrets Manager; optional one-way TLS for integrity |
| Rollout on a running stack | TLS and plaintext can't share port 9090, so drain and switch every lobby and game server at once (or use a second port during the switch) | the interceptor runs in log-only mode first, then enforces: a normal rolling deploy |
| Afterwards | rerun the CI stack test and the kill test, and update the evidence log | the same |
| Rough total | 1–1.5 days with our own CA, 2–2.5 days with Private CA | 1–1.5 days, plus about half a day for one-way TLS |
| Monthly cost (see "Cost in AWS") | USD 0–5 with our own CA; USD 4 (informal) to USD 55–70 (public) with Private CA | USD 0–2 |

If the stack is torn down between sessions, there is no rollout: deploy everything at once. Waiting until the end adds about half a day to a day compared with doing it now, mostly for AWS and the rollout. That stays true only while the seam stays narrow. It grows with every new gRPC caller (Stage 6 relays, a Stage 7 `doodle.ai.v1`), and with any handler that comes to rely on the caller's identity.

The upgrade becomes its own task. It would cover: `make up` still starts with no manual steps; the tests show a heartbeat without credentials, with a wrong `server_id`, with an expired or wrongly signed credential, and an `AllocateRoom` or `DrainServer` from anyone but the lobby are all rejected; rejected calls are logged (never the credential itself) and counted in a metric named per `docs/observability.md`; and `docs/setup.md` and section 11 of the design doc describe the keys or certificates.
