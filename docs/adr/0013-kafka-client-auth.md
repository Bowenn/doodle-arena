# ADR 0013: Authenticating Kafka clients and restricting each topic to its producer and consumers

- Status: Accepted (Option A with Option C's network split; implementation in task 5.5, before 5.2 exposes the stack)
- Date: 2026-10-18
- Deciders: Bowen

## Context

The local Kafka (`apache/kafka:4.2.2`, one KRaft node in `infra/docker-compose.yml`) has three listeners, all `PLAINTEXT`: `INTERNAL` (9092, every service), `CONTROLLER` (9093, the KRaft quorum) and `EXTERNAL` (9094, published on `127.0.0.1` for host tools such as `tools/eventctl`, `scripts/fake-events.sh` and `make pipeline-check`). No authorizer is configured. Topics are created only by the `kafka-topics` job (`infra/kafka/create-topics.sh`), because auto-creation is off, but any client that reaches 9092 can do everything else.

ADR 0006 accepted the same situation for the lobby–game-server gRPC, because the attacks there cost availability, not room access or money. Kafka is different. Since task 4.4, the marketplace pays coins from `MatchFinished` on `game.matches.v1`, and since task 4.2 it mints drawings from `DrawingApproved` on `assets.drawings.v1`. The topic is the only thing that says "the game server decided this", so the money path trusts whoever can write to it.

### The threat today

What any process on the Compose network (or, in AWS, anything that reaches the broker) can do:

- **Mint coins.** Publish a `MatchFinished` with a fresh `match_id` and up to 8 results. The marketplace pays each one once (`reward:<match_id>`), but nothing limits how many match ids a forger invents. Task 4.4's check (more results than a room holds is a contract violation, design doc 7.9) caps one event at 8 players × 30 coins, not the total.
- **Mint drawings.** Publish a `DrawingApproved` for any `image_key` and `initial_owner_id`, and the marketplace creates the drawing and its ownership. A rejected drawing can be "approved" this way.
- **Forge the economy's own events.** Publish `SaleCompleted` or `CoinsCredited` on the `market.*` topics. Today only the notifier (later) and analytics (Stage 6) read them, so the damage is webhooks and fraud metrics that lie, not balances. That changes if any consumer ever acts on them.
- **Make a consumer skip events.** Join the consumer group `marketplace` (or `assets`, `lobby-leaderboard`), take partitions in the rebalance, and commit offsets past the records it received. The real marketplace then never sees those `MatchFinished` or `DrawingApproved` records: rewards and drawings are silently lost, not duplicated. Committing offsets with the admin API (`AlterConsumerGroupOffsets`) does the same without even joining.
- **Destroy data.** Delete a topic, delete records, or set `retention.ms` to a few milliseconds through the admin API. The outboxes (ADRs 0009, 0010, D36) protect events until Kafka acknowledges them, not afterwards.
- **Read everything.** Every event, including each round's `word` and every player's id. Reading is the least of these problems: none of it is secret after the round ends.

Who is "on the network" locally: every container, including the gateway (the internet-facing one), the AI service, auth, and the assets web process and Celery worker (which render user-submitted drawings, the likeliest place for a remote-code-execution bug, as ADR 0006 notes).

### Requirements for any option

- **One principal per service, enforced per topic.** The "Producer" and "Consumers" columns of the event catalog (design doc 6.2) become the access list. Authentication alone ("some service") doesn't help: the AI service, authenticated, must still be unable to write `game.matches.v1`.
- **Consumer groups are protected too**, not only topics: only the marketplace may join or commit for group `marketplace`.
- **Services get no admin rights.** Creating topics, altering configs, deleting records and managing ACLs belong to one bootstrap identity used by the init job and humans.
- **`make up` keeps working with no manual step**, on a fresh clone, in a worktree (`ENV_FILE` pointing at the main checkout's `.env`, as today) and in CI's stack job.
- **Testcontainers tests keep working** in all three languages without each test learning about credentials.
- **Rotation without a coordinated restart**, like the JWT and ticket keys.
- **No new dependency on the hot path.** A produce or a fetch must not wait on auth or any other service (ADR 0001).
- **A path to AWS** for each shape in ADR 0003 (still Proposed): a Kafka container on EC2 or Fargate (its Options A and B), or MSK (its Option C).

### What authentication and ACLs can't do

ACLs decide who may write to a topic, not what they write. An authenticated game server can still publish a wrong `MatchFinished`, through a bug or because it is compromised. The game server is authoritative for scoring by design (CLAUDE.md), so this is accepted, and the marketplace's contract checks (task 4.4's 8-player limit, placements, repeated players) stay as plausibility checks. They stop being the only defence.

## The ACLs (the same for every option)

Kafka's ACLs are the actual control. The options below only differ in how a client proves which principal it is. Every option maps a client to one of these principals, so the table doesn't change when the mechanism does.

With KRaft, the authorizer is `org.apache.kafka.metadata.authorizer.StandardAuthorizer`, ACLs live in the metadata log, and `allow.everyone.if.no.acl.found` stays at its default `false`: anything not allowed below is denied.

| Topic | Write (producer) | Read (consumer) | Consumer group | Stage |
|---|---|---|---|---|
| `game.rounds.v1` | `game-server` | `assets` | `assets` | 2 |
| `game.matches.v1` | `game-server` | `marketplace`, `lobby` | `marketplace`, `lobby-leaderboard` | 2 |
| `assets.drawings.v1` | `assets` | `marketplace` | `marketplace` | 3 |
| `market.listings.v1` | `marketplace` | `assets` (Stage 6) | `assets` | 4 |
| `market.sales.v1` | `marketplace` | `notifier`, `analytics` | `notifier`, `analytics` (its Flink group) | 4 |
| `market.wallet.v1` | `marketplace` | `notifier` | `notifier` | 4 |
| `notifier.dlq.v1` | `notifier` | humans (`dev-tools`) | none | 4 |
| `auth.users.v1` | `auth` | `notifier` | `notifier-auth` | 4 (ADR 0015) |
| `live.guesses.v1` | `game-server` (relays) | `analytics` | `analytics` | 6 |
| `live.top-guesses.v1` | `analytics` | `game-server` | one group per relay, or none (assigned partitions) | 6 |

Notes on the table:

- **Grant only what the code uses today.** A Stage 6 row is added with its task, not now. A principal for a service that doesn't use Kafka yet (`notifier`, `analytics`) is created with its first consumer.
- **Operations.** A producer gets `Write` on its topics. A consumer gets `Read` on its topics and `Read` on its group's literal name. `Read` and `Write` imply `Describe`. Use explicit `--operation` flags in `kafka-acls.sh`, not `--producer`, which also grants `Create` on the topic.
- **Idempotent producers need nothing extra.** franz-go's default producer, the assets relay (`enable.idempotence`) and the marketplace (`enable.idempotence: true`) are all idempotent. Since Kafka 3.0 (KIP-679), `Write` on a topic implies the old cluster-level `IdempotentWrite`. Nobody uses Kafka transactions today. A Flink exactly-once sink (Stage 6) would need `Write` on a `PREFIXED` `TransactionalId` such as `analytics-`.
- **Two game servers share one principal**, as ADR 0006's "per-class identity". They already share `room:*` and `round-logs`, and one game server impersonating the other gains nothing on Kafka.
- **Assets' credential goes only to the processes that use Kafka**: the round consumer (`Read` on `game.rounds.v1`, group `assets`) and the outbox relay (`Write` on `assets.drawings.v1`). The web process, the Celery worker and beat get no Kafka credential at all, so a rendering exploit in the worker can't reach Kafka with the service's identity. Splitting them into `assets-consumer` and `assets-relay` would be stricter still, but a compromised relay can already publish any `DrawingApproved`, so the gain is small.
- **`dev-tools`** is the host tools' principal, on the `EXTERNAL` listener: `Read` on every topic (eventctl assigns partitions and uses no group) and `Write` on `game.rounds.v1` and `game.matches.v1` for `fake-events.sh` and `pipeline-check`. It can mint coins, which is its purpose. It exists only locally and in CI, never in AWS.
- **`admin`** is the super user (`super.users`). The init job and humans use it to create topics, SCRAM users and ACLs. No service has it.

## Options

### Option A: SASL/SCRAM-SHA-512, one username and password per service

Every client listener becomes `SASL_PLAINTEXT` locally (`SASL_SSL` in AWS). Each service gets a Kafka username (its principal) and a random password, exactly like its Postgres user today.

- **Broker.** `KAFKA_LISTENER_SECURITY_PROTOCOL_MAP: INTERNAL:SASL_PLAINTEXT,EXTERNAL:SASL_PLAINTEXT,CONTROLLER:…`, `KAFKA_SASL_ENABLED_MECHANISMS: SCRAM-SHA-512`, the authorizer and `super.users`. SCRAM credentials are stored in the KRaft metadata log, so they are added, changed and removed on a running broker (`kafka-configs.sh --alter --entity-type users --entity-name marketplace --add-config 'SCRAM-SHA-512=[password=…]'`). Rotation: set the new password on the broker, then restart the service with it; existing connections stay up.
- **Bootstrapping the first credential** is the one awkward part. The init job needs `admin` to create the other users, and `admin`'s own credential must exist before anything can authenticate. KRaft solves this at format time (`kafka-storage.sh format --add-scram`, KIP-900). The `apache/kafka` image formats storage itself on first start, and we didn't find an environment variable for it, so the spike must pick one of two ways. Format storage ourselves in a small entrypoint wrapper. Or keep the `CONTROLLER` listener `PLAINTEXT`, bound to `localhost` inside the container, with `User:ANONYMOUS` as a super user, and run the init commands inside the Kafka container. The second way is simpler but makes anything that reaches the controller listener all-powerful, and clients can send some admin requests to controllers directly (`--bootstrap-controller`, KIP-919), so it has to really be `localhost`-only.
- **Clients** (configuration only, no new library):
  - Go (franz-go, game-server, lobby, eventctl): `kgo.SASL(scram.Auth{User: u, Pass: p}.AsSha512Mechanism())` from `github.com/twmb/franz-go/pkg/sasl/scram` (part of franz-go), plus `kgo.DialTLSConfig(…)` in AWS. About 15 lines in `services/game-server/internal/events/clients.go` and `services/lobby/internal/leaderboard/consumer.go`.
  - Python (confluent-kafka, assets consumer and relay): `security.protocol=SASL_PLAINTEXT`, `sasl.mechanism=SCRAM-SHA-512`, `sasl.username`, `sasl.password` in the two config dicts (`drawings/consumer.py`, `drawings/relay.py`). librdkafka has SCRAM built in.
  - Java (Spring Kafka, marketplace): `spring.kafka.security.protocol=SASL_PLAINTEXT`, `spring.kafka.properties.sasl.mechanism=SCRAM-SHA-512` and `spring.kafka.properties.sasl.jaas.config=org.apache.kafka.common.security.scram.ScramLoginModule required username="…" password="…";` in `application.yaml`, from environment variables.
- **In AWS.** A Kafka container (ADR 0003 A or B): the same broker config with a `SASL_SSL` listener and a server certificate only (no client certificates), and passwords in SSM Parameter Store (free) or Secrets Manager. MSK (ADR 0003 C): MSK supports SCRAM-SHA-512 on its own port (9096), always over TLS. Each user is a Secrets Manager secret whose name starts with `AmazonMSK_`, encrypted with a customer-managed KMS key and associated with the cluster. Kafka ACLs work on MSK as here, with `allow.everyone.if.no.acl.found=false` in the cluster configuration. The client code is the same as locally; only `security.protocol` and the CA change. (Check before relying on it that MSK Express brokers, the only ones with Kafka 4.2, support SCRAM; AWS's broker-types page lists IAM, SCRAM and mTLS, but it wasn't clear for which broker type.)

- Pros: the same pattern as everything else the project already does: one generated secret per service in `.env`, one user per service. The smallest client change (configuration in all three languages, nothing to install). Credentials change on a running broker. Debugging stays readable: a wrong password is `SASL authentication failed`, a missing ACL is `TOPIC_AUTHORIZATION_FAILED` naming the topic. SCRAM never sends the password, even on a plaintext connection. It carries over unchanged to a Kafka container in AWS, and to MSK with Secrets Manager.
- Cons: locally the traffic stays unencrypted and unauthenticated after the handshake: anything with `NET_RAW` on the bridge network could read records or, with more effort, tamper with a connection (the same weakness ADR 0006's spike found for tokens on plaintext). In AWS that is fixed by `SASL_SSL`, which needs a server certificate. Passwords are long-lived shared secrets that must be distributed and rotated. On MSK each user costs a Secrets Manager secret (USD 0.40 a month) plus one KMS key (USD 1 a month).

### Option B: mutual TLS, one client certificate per service

Every client listener becomes `SSL` with `ssl.client.auth=required`. The principal is taken from the certificate's subject: `ssl.principal.mapping.rules=RULE:^CN=([a-z-]+),.*$/$1/,DEFAULT` turns `CN=marketplace,O=doodle` into `User:marketplace`, so the ACL table is identical.

- **Broker.** A PEM keystore and truststore (`ssl.keystore.type=PEM`, supported since Kafka 2.7), so no Java keystores. The `CONTROLLER` listener can use TLS too, so there is no `ANONYMOUS` super user and no bootstrap problem: `admin` is just a certificate.
- **Locally.** `scripts/gen-internal-certs.sh` (the script ADR 0006 Option A described and never built) creates a CA and one key and certificate per principal under `secrets/` (gitignored), once, from `make up`. Compose mounts each service's files. Rotation: issue a new certificate from the same CA and restart the service; Kafka can also reload the broker's own keystore without a restart.
- **Clients:**
  - Go (franz-go): `kgo.DialTLSConfig(&tls.Config{Certificates: …, RootCAs: …})`, standard library only.
  - Python (confluent-kafka): `security.protocol=SSL`, `ssl.ca.location`, `ssl.certificate.location`, `ssl.key.location`.
  - Java (Spring Kafka): a Spring Boot SSL bundle (`spring.ssl.bundle.pem.kafka.*`, then `spring.kafka.ssl.bundle=kafka`), or `spring.kafka.ssl.*` with `ssl.keystore.type=PEM`.
- **In AWS.** A Kafka container: the same certificates, from our own CA with its key in Secrets Manager (ADR 0006's cost table: almost free), or AWS Private CA (USD 50 a month in short-lived mode). MSK supports mTLS client authentication (port 9094) only with certificates from AWS Private CA, so on MSK it costs a Private CA.

- Pros: encryption, integrity and identity in one mechanism, locally as well as in AWS, so nothing on the network can read or alter records. No passwords to leak in logs or environment dumps. It builds the CA that ADR 0006 Option A needs, so the gRPC upgrade becomes much cheaper (and the reverse). Strong interview material: SPIFFE-style service identity, principal mapping rules.
- Cons: the most moving parts: a CA, issuance, expiry and rotation, in every environment. A certificate mistake is an opaque handshake failure in every client (ADR 0006's spike saw the same for gRPC). The TLS handshake adds cost per connection (small: clients keep connections open). Testcontainers tests that want a secured broker need generated certificates. On MSK it needs AWS Private CA.

### Option C: network isolation only

Keep Kafka unauthenticated and limit who can reach it, as ADR 0006 did for gRPC.

- **Locally.** Put Kafka on its own Compose network and attach only the containers that use it: the game servers, the lobby, the assets consumer and relay, the marketplace, the topic job and later the notifier. The gateway, auth, the AI service and the assets web process and Celery worker leave it. Unlike gRPC in ADR 0006, this works in Compose without binding tricks, because Kafka's clients reach it by name and the excluded containers have no other route to it.
- **In AWS.** Security groups: the broker's group accepts its port only from the groups of the services above. With EC2 hosts running Compose (ADR 0003 A), containers on one host share the host's group, so this separates nothing between them. MSK supports unauthenticated access too.

- Pros: no code, no secrets, no certificates. Worth doing under every option, as defense in depth: it takes the internet-facing gateway and the rendering worker off the broker's network. It removes the most likely attackers (a compromised gateway, AI service or Celery worker).
- Cons: it doesn't separate topics. Every remaining client can still write every topic: a compromised lobby (which only needs to read `game.matches.v1`) can mint coins, and any of them can hijack the marketplace's consumer group or delete topics. The "Producer" column of the catalog stays a convention. Under ADR 0003 A it protects almost nothing in AWS. It is the "trusted network" model that ADR 0001 rejected for user requests, now on a money path.

### Option D: MSK IAM authentication in AWS, SCRAM locally

Only if ADR 0003 picks MSK (its Option C). Clients authenticate with their ECS task role (SigV4, `AWS_MSK_IAM` or `OAUTHBEARER`, port 9098). IAM policies replace Kafka ACLs: `kafka-cluster:Connect`, `WriteData` and `ReadData` on topic ARNs, `AlterGroup` and `DescribeGroup` on group ARNs. The table above becomes one Terraform policy per service. Locally, where IAM doesn't exist, clients use Option A.

- **Clients:** Java needs `software.amazon.msk:aws-msk-iam-auth` (`IAMLoginModule`, `IAMClientCallbackHandler`). Go can use franz-go's `pkg/sasl/aws` (`ManagedStreamingIAM`) with the AWS SDK's credentials. Python needs `aws-msk-iam-sasl-signer-python` and an `oauth_cb` with `sasl.mechanism=OAUTHBEARER`.
- Pros: no passwords or certificates anywhere in AWS: the task role is the identity, and it rotates itself. Permissions are Terraform, reviewed like the rest of the infrastructure. A good AWS interview story.
- Cons: two authentication paths, one per environment, in all three languages, so the AWS path is never exercised by `make up` or CI. A new AWS-specific dependency in every service. Only applies to MSK, the most expensive ADR 0003 option, which its own suggestion doesn't favour.

### Rejected without a full write-up

- **SASL/PLAIN:** sends the password itself (so it needs TLS everywhere), and Kafka's built-in PLAIN users live in a static broker file that needs a restart to change. SCRAM is strictly better here.
- **SASL/OAUTHBEARER with auth as the issuer:** service tokens from our own auth service. It puts auth on the path of every Kafka reconnection and token refresh, which ADR 0001 rules out, and adds a token flow for something a static identity covers.

## Should events carry a producer proof?

No, once ACLs exist. Every topic in the catalog has exactly one producing service, so with one `Write` principal per topic, "this record is on `game.matches.v1`" already means "the game server wrote it". `EventMeta.producer` stays an informational, self-asserted field for logs and debugging; consumers shouldn't check it, because it proves nothing.

A signature on each event (for example Ed25519 over the payload, in a header) would only add protection against someone who has `Write` on the topic without being its producer, which is exactly what ACLs prevent, or against a compromised broker or `admin`, which can also delete everything. A compromised producer holds its own signing key, so a signature doesn't help there either. It would cost a key per producer, verification in every consumer and a `proto/` or header contract change. Revisit only if a topic ever gets several unrelated producers.

## What tips the choice

- **Option A** if the priority is the smallest change that makes the catalog's producer column real, with the same secret handling as Postgres and a direct path to both AWS shapes.
- **Option B** if the priority is encryption locally and a shared CA with ADR 0006, at the price of certificate operations in every environment.
- **Option C alone** if Kafka stays a known gap like the gRPC in ADR 0006. That was reasonable for availability; here it leaves coin minting open, so it should at least be dated and tied to a trigger.
- **Option D** only after ADR 0003 picks MSK, and then as the AWS half of Option A.
- The ACL table and the Compose network split from Option C apply whatever is chosen.

## Recommendation

Option A (SCRAM-SHA-512 per service plus the ACL table), together with Option C's network split, and `SASL_SSL` with a server certificate in AWS:

- It closes the actual hole, the money path, with configuration-only client changes and the secret pattern the stack already uses.
- ACLs, not the mechanism, are the control. Because principals are plain service names, switching later to mTLS (B) or MSK IAM (D) keeps the table and only changes how clients prove their name. Option B can come together with ADR 0006's upgrade, when the CA is built anyway.
- Unlike ADR 0006, the threat here is money and drawings, which CLAUDE.md lists as critical paths, and the fix is cheap (about a day, below). Real money also enters with Stripe in task 4.5: free coins devalue coins someone paid for, even in test mode.

## Local dev, CI and rollout under the recommendation

- **Secrets.** `scripts/dev-up.sh` already replaces `change-me` placeholders in `.env` with random values. Add `KAFKA_ADMIN_PASSWORD`, `KAFKA_GAME_SERVER_PASSWORD`, `KAFKA_LOBBY_PASSWORD`, `KAFKA_ASSETS_PASSWORD`, `KAFKA_MARKETPLACE_PASSWORD` and `KAFKA_DEV_TOOLS_PASSWORD` to `.env.example`. Nothing new under `secrets/` (that stays for keys and, under Option B, certificates). Worktrees keep using `ENV_FILE`.
- **Init job.** The `kafka-topics` job becomes `kafka-init`: as `admin`, it creates the topics, the SCRAM users and the ACLs from one declarative list next to `create-topics.sh`. Every step is idempotent (`--if-not-exists`; setting a SCRAM password again is a no-op; adding an existing ACL is a no-op). Removing a grant that left the list is manual at first; the init job can print ACLs that aren't in the list.
- **Broker healthcheck** uses a client properties file with the `admin` credential, or the `localhost` controller listener, whichever the bootstrap spike picks.
- **Host tools.** `eventctl`, `fake-events.sh` and `pipeline-check.sh` read `KAFKA_DEV_TOOLS_PASSWORD` from `ENV_FILE`. A missing password is a clear error, not a hang.
- **Testcontainers.** Integration tests keep their unauthenticated brokers: they test the services' logic, not Kafka's authorizer. Each service builds its client config in one function that adds SASL only when a username is set, unit-tested for both cases. CLAUDE.md and `docs/setup.md` say so.
- **CI's stack job** runs `make up` as today, plus a new `make kafka-acl-check` (in `scripts/`): a client with no credential is refused; `lobby` can't write `game.matches.v1`; `assets` can't join group `marketplace`; `marketplace` can't create a topic; and the happy path (`pipeline-check`) still passes. These negative checks are the real test of this ADR.
- **Telemetry.** Services already log produce and consume errors; an authorization failure must log the topic or group and the principal, never the password. Route the broker's `kafka.authorizer.logger` (denials at INFO) to stdout so denials reach Loki, and count client-side authentication and authorization failures in a metric named per `docs/observability.md`. An `admin` login outside the init job is worth an alert later.
- **Rollout.** Locally and in a torn-down AWS stack: restart everything at once. On a running stack: add a `SASL` listener on a new port beside `PLAINTEXT`, create the users and ACLs while `PLAINTEXT` still works (its `ANONYMOUS` principal would need temporary ACLs, or switch the authorizer on last), move the services one by one, then remove `PLAINTEXT`.
- **Rough size:** about one day: the broker config and bootstrap (the uncertain part, so spike it first), the init job, four client configs (game-server, lobby, assets, marketplace) plus eventctl, the ACL check and the docs.

## Where it belongs in the roadmap

Task 5.5 already lists "per-service credentials outside Postgres, Redis ACL users per key prefix and a SeaweedFS identity per bucket, so data ownership is enforced, not just a convention". Kafka users and topic ACLs are the same idea for the fourth shared store, with the same mechanics (generated `.env` passwords, an idempotent init job, a negative check in the stack job), so they belong in that bullet.

One difference: Redis and SeaweedFS ownership is about one service corrupting another's data by mistake; Kafka is also the coin and drawing mint. 5.5 comes after 5.2 (AWS), so either do this piece of 5.5 before 5.2 exposes the stack to anyone outside the team, or make it an explicit 5.2 prerequisite in the roadmap. Until then the 8-player check from task 4.4 stays the stopgap, and design doc sections 7.9 and 11 keep naming the gap.

## When to decide

- **Nothing in Stage 4 depends on it.** No option changes `proto/` or any event; each one wraps client construction (one function per service) and the broker config.
- **Before 5.2 goes public**, as above. ADR 0003's choice decides only the AWS half (container plus `SASL_SSL`, or MSK with SCRAM or IAM), not the local design.
- **Each new Kafka client** (the notifier's `market.sales.v1` consumer in task 4.7, analytics and relays in Stage 6) adds a principal and rows to the table. The later this lands, the more clients the switch touches.

## Decision

Option A: SASL/SCRAM-SHA-512 with one Kafka user per service, and the ACL table above enforced by `StandardAuthorizer` with deny by default. Together with Option C's network split: Kafka moves to its own Compose network, joined only by its clients. Locally the listeners are `SASL_PLAINTEXT`; in AWS they are `SASL_SSL` with a server certificate (a Kafka container), or MSK's SCRAM listener if ADR 0003 picks MSK. Option D (MSK IAM) stays possible later as the AWS half, and Option B (mTLS) can replace SCRAM when ADR 0006's upgrade builds a CA; both keep the same principals and the same table.

Why:

- **It closes the money path.** Only the game server can write `game.matches.v1` and only assets can write `assets.drawings.v1`, so minting coins or drawings needs that service's credential, not just a foothold on the network. Only each consumer can join or commit for its own group, and no service can delete topics or records.
- **It is the smallest change that does that**: configuration in all three client libraries, and the same generated-password pattern as the Postgres users.
- **The ACLs are the control, not the mechanism**, so this doesn't lock out mTLS or IAM later.
- **The network split costs nothing** and takes the gateway, auth, the AI service and the assets web process and Celery worker off the broker's network entirely.

Rejected: B for now (certificate operations in every environment, better done together with ADR 0006's upgrade), C alone (it doesn't separate topics, so any Kafka client could still mint coins), and D as the only mechanism (it exists only on MSK and would leave local dev unauthenticated). Events carry no producer proof (see above).

## Consequences

- **Task 5.5 implements it, before task 5.2 exposes the stack** to anyone outside the team (roadmap). Until then Kafka stays open, and task 4.4's 8-player check remains the only bound on a forged `MatchFinished`. Design doc sections 7.9 and 11 name the gap.
- **The work, as described in "Local dev, CI and rollout" above:** spike the first-credential bootstrap first; the `kafka-init` job (topics, SCRAM users, ACLs from one declarative list); the six `KAFKA_*_PASSWORD` entries in `.env.example`; client config in game-server, lobby, assets (consumer and relay only), marketplace and eventctl; the host scripts reading `KAFKA_DEV_TOOLS_PASSWORD`; the Kafka-only Compose network; `make kafka-acl-check` in CI's stack job; denials logged and counted; `docs/setup.md`, CLAUDE.md's commands and the design doc updated.
- **Every new Kafka client adds a principal and its rows** to the ACL table and the init job's list, in the same task: the notifier (4.7), analytics and relays (Stage 6). A Flink exactly-once sink also needs a prefixed `TransactionalId` grant.
- **Integration tests stay on unauthenticated brokers**; each service's client-config function is unit-tested with and without credentials, and the stack job's negative checks test the ACLs themselves.
- **Still trusted:** an authenticated producer's content. A compromised or buggy game server can still publish wrong results, so the marketplace keeps its contract checks. Locally the traffic is unencrypted; that is accepted for a single-host bridge network and fixed by `SASL_SSL` in AWS.
- **Watch:** the `admin` credential is all-powerful. It's for humans only, on the broker's localhost ADMIN listener, and every use raises KafkaAdminUsed (implementation notes below).
- Interview angle: authorization per topic and per consumer group, why consumer-group hijacking is as dangerous as forged events, why the mechanism (SCRAM, mTLS, IAM) is swappable while the ACLs stay, and why a topic ACL makes a per-event signature unnecessary.

## Implementation notes (task 5.5)

- **The first credential.** `infra/kafka/broker.sh` replaces the image's start script: it writes `server.properties` and formats the storage once with `kafka-storage.sh format --add-scram` for `admin` (KIP-900). The `CONTROLLER` listener can't use SCRAM, because the credentials live in the metadata log the controller serves. So it is SASL/PLAIN with a static `admin` user, on `localhost` only. No `ANONYMOUS` super user exists anywhere. The data moved to `/var/lib/kafka/data/kraft`, so a volume formatted before this task is left alone instead of starting without an admin.
- **More principals than the table above.**
  - `notifier`: reads `market.sales.v1`, `market.wallet.v1` and `auth.users.v1` in groups `notifier` and `notifier-auth`, and writes `notifier.dlq.v1`.
  - `auth`: the `auth-relay` writes `auth.users.v1`.
  - `kafka-metrics`: Describe on every topic and group, and on the cluster, for broker-side lag.
  - `dev-tools`: Read on every topic, Write on `game.rounds.v1` and `game.matches.v1`, and Read on groups starting with `dev-tools-`. It exists only locally and in CI; in AWS its password is unset, so `init.sh` skips it.
  - The list is `GRANTS` in `infra/kafka/init.sh`.
- **`make pipeline-check` moved** out of the assets web container, which no longer has Kafka. It runs as a one-off container of `assets-consumer`'s with the `dev-tools` credential, the one user besides the game server that may write `game.rounds.v1`.
- **AWS.** Terraform's `tls` provider makes a CA and the broker's certificate (`infra/terraform/stack/kafka.tf`; names `data.doodle.internal`, `kafka`, `localhost`). The data host writes the key to a file at boot; tasks get the CA in `KAFKA_CA_PEM` and their password from SSM. The broker's own inter-broker client needs the CA too: it checks its SSL settings at startup. This was found with a local `SASL_SSL` broker before any AWS run. All three client libraries were checked over TLS with the CA as a PEM string: each one connects with the right CA and refuses an unrelated one.
- **Checked in AWS** (2026-10-20): a bot match went from the game server to the marketplace, the lobby, assets and the notifier over `SASL_SSL`, each service as its own user. ECS doesn't wait for the data host's `kafka-init` (2 minutes there), so the lobby was refused for 2 s at startup and then carried on. A Spring Kafka listener container stops for good on an authorization error by default, so the marketplace retries them every 10 s (`spring.kafka.listener.auth-exception-retry-interval`). franz-go and librdkafka retry on their own.
- **Telemetry.**
  - The broker logs every denial at INFO on stdout (`kafka.authorizer.logger`: principal, operation, resource; `make logs s=kafka`).
  - Clients log their produce and consume errors as before; Kafka itself never prints a password (`sasl.jaas.config = [hidden]`).
  - The marketplace's Java agent already exports `kafka_consumer_failed_authentication_total` and `kafka_producer_failed_authentication_total`.
- **Counted at the broker, and admin made countable** (follow-up to task 5.5).
  - The broker runs the OpenTelemetry Java agent: its logs (each denial with its principal) go to Loki, and JMX rules export failed and successful logins per listener and the authorizer's denied rate (`docs/observability.md`).
  - Counting at the broker covers every client, including the one a client-side counter can't: a client that isn't ours.
  - The authorizer's denied rate also counts topics it quietly filters out of metadata answers, so it never reads zero. KafkaAuthFailures therefore alerts on failed logins and on logged denials, for 5 minutes in a row.
- **Three super users.** Idle, the broker made 47 requests a minute as `admin` (its own controller heartbeats), so "admin must not be used" needed separate identities:
  - `broker`, the broker itself;
  - `kafka-init`, the init job;
  - `admin`, humans only.
- **admin has no SCRAM credential** (an older volume's is deleted by `kafka-init`). It exists only as a PLAIN user on a localhost ADMIN listener inside the broker container (`kafka-admin`), so it can't be used over the network at all, and `make kafka-acl-check` proves that.
  - Every admin login counts on that listener and raises KafkaAdminUsed.
  - Rejected: logging the authorizer's allowed requests at DEBUG with a log4j filter that keeps only admin's. The agent captures log events before that filter, so every authorized request would have reached Loki.
