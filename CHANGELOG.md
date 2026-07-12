# Changelog

## [0.15.3](https://github.com/fclairamb/dbbat/compare/v0.15.2...v0.15.3) (2026-07-12)

Implements seven backlog specs in one squash-merged PR ([#240](https://github.com/fclairamb/dbbat/issues/240)) ([caa73cb](https://github.com/fclairamb/dbbat/commit/caa73cb7ed99742556f1179f1c9500084fa85bc1)); the individual changes are broken out below.

### Features

* **ui:** query detail breadcrumb now reads `Queries › <sql-preview>` — a link back to the queries list plus the first ~40 chars of the SQL — instead of a generic "Details"; the parent crumb now appears on every detail route and a bare UUID no longer collapses to "Details" ([#240](https://github.com/fclairamb/dbbat/issues/240))
* **ui:** grants list and grant-definitions always show the applied limits (`9 / 100 queries`, `169.8 MB / 1 GB`) with a usage bar, a warning colour ≥80%, destructive ≥100%, and an explicit `unlimited` marker when no limit is set ([#240](https://github.com/fclairamb/dbbat/issues/240))
* **api:** rename the connection-URL password placeholder `{API_KEY}` → `{DBBAT_KEY}` so it unambiguously names a dbbat-issued `dbb_…` token ([#240](https://github.com/fclairamb/dbbat/issues/240))


### Bug Fixes

* **ui:** the grant-definition edit dialog now opens pre-filled with the definition's current values instead of an empty form (which silently blanked the definition on save) ([#240](https://github.com/fclairamb/dbbat/issues/240))
* **ui:** the "New Definition" dialog now opens blank on consecutive opens instead of retaining the previously-submitted values ([#240](https://github.com/fclairamb/dbbat/issues/240))
* **oracle:** surface grant/auth denials as a clean error — no active grant → `ORA-01045`, bad credentials → `ORA-01017` — instead of tearing the socket down and letting the client report a generic `ORA-12566` / `ORA-03113` (root cause: the auth-reject frame used legacy TNS framing that v315+ clients misread) ([#240](https://github.com/fclairamb/dbbat/issues/240))
* **oracle:** rename the misleading `isPrintableASCII` helper (it only accepted the Oracle identifier set) and fix three latent call sites that truncated dotted usernames or rejected special-character passwords — the same class of bug as [#235](https://github.com/fclairamb/dbbat/issues/235) ([#240](https://github.com/fclairamb/dbbat/issues/240))

## [0.15.2](https://github.com/fclairamb/dbbat/compare/v0.15.1...v0.15.2) (2026-07-12)


### Features

* **ui:** hide the Active Connections stat card when there are none ([#238](https://github.com/fclairamb/dbbat/issues/238)) ([242bd8c](https://github.com/fclairamb/dbbat/commit/242bd8cd1f11ce206bd225b1e18068e23fbe7ead))


### Bug Fixes

* **deps:** update docusaurus monorepo to v3.10.2 ([#236](https://github.com/fclairamb/dbbat/issues/236)) ([25f4adb](https://github.com/fclairamb/dbbat/commit/25f4adb43ca67c66e60cbe79ef8f9a85a69724f1))
* **oracle:** harden Phase 2 rewrite against big-CLR-chunk connect strings ([#238](https://github.com/fclairamb/dbbat/issues/238)) ([242bd8c](https://github.com/fclairamb/dbbat/commit/242bd8cd1f11ce206bd225b1e18068e23fbe7ead))

## [0.15.1](https://github.com/fclairamb/dbbat/compare/v0.15.0...v0.15.1) (2026-07-10)


### Bug Fixes

* **ci:** prevent shell injection from commit message in image workflow ([#233](https://github.com/fclairamb/dbbat/issues/233)) ([a436a05](https://github.com/fclairamb/dbbat/commit/a436a05bac6e0b593fbc81272d28587f5e2991f7))
* **oracle:** handle dotted usernames in OCI/sqlplus auth (both phases) ([#235](https://github.com/fclairamb/dbbat/issues/235)) ([d822022](https://github.com/fclairamb/dbbat/commit/d822022fbb84466f473b3ac7db9f98ae73244803))

## [0.15.0](https://github.com/fclairamb/dbbat/compare/v0.14.0...v0.15.0) (2026-07-09)

Implements five backlog specs in one squash-merged PR ([#230](https://github.com/fclairamb/dbbat/issues/230)) ([42c4e37](https://github.com/fclairamb/dbbat/commit/42c4e3713c95091fef3b51a15dd54489813300c8)); the individual changes are broken out below.

### Features

* **ui:** admin user-management UI — edit users and promote/demote admin rights from the users page, guarded so the last admin can't be demoted or deleted (UI lock plus a backend `409` on update/delete) ([#230](https://github.com/fclairamb/dbbat/issues/230))

### Bug Fixes

* **oracle:** make sqlplus / OCI instant client work through the proxy — root-caused the long-standing stall to a malformed wide-encoding AUTH challenge (not the TCP-urgent OOB break probe that was long assumed), fixing four wide-encoding bugs; works even over an OOB-dropping network path ([#230](https://github.com/fclairamb/dbbat/issues/230))
* **config:** accept the documented `DBB_SLACK_SIGNING_SECRET` env var for the Slack signing secret, with the legacy `DBB_SLACK_NOTIFY_SIGNING_SECRET` kept as an accepted alias (canonical wins if both are set) ([#230](https://github.com/fclairamb/dbbat/issues/230))

### Documentation

* document the three Slack interactivity deployment shapes and Socket Mode (`DBB_SLACK_NOTIFY_APP_TOKEN`) for gated deployments, plus a startup warning when the inbound endpoint must be reachable from Slack ([#230](https://github.com/fclairamb/dbbat/issues/230))
* document HTTPRoute (Gateway API) exposure on the website and fix the Docusaurus build ([#230](https://github.com/fclairamb/dbbat/issues/230))

## [0.14.0](https://github.com/fclairamb/dbbat/compare/v0.13.0...v0.14.0) (2026-07-08)


### Features

* **api:** add Slack interactive grant approval (Approve/Deny buttons) ([#223](https://github.com/fclairamb/dbbat/issues/223)) ([516d4ca](https://github.com/fclairamb/dbbat/commit/516d4ca7b1152dcd5122a49dea071ef46b5961ea))
* **api:** add Slack Socket Mode transport for Approve/Deny interactions ([#229](https://github.com/fclairamb/dbbat/issues/229)) ([d1d33d4](https://github.com/fclairamb/dbbat/commit/d1d33d4c80c60cbb6b2cdeeddade0b095359a701))


### Bug Fixes

* **deps:** update module golang.org/x/crypto to v0.54.0 ([#226](https://github.com/fclairamb/dbbat/issues/226)) ([d53ede1](https://github.com/fclairamb/dbbat/commit/d53ede1b36bf573498d87982e11ab1ca24f1782f))
* **deps:** update module golang.org/x/text to v0.39.0 ([#221](https://github.com/fclairamb/dbbat/issues/221)) ([c10f410](https://github.com/fclairamb/dbbat/commit/c10f4101bc9b6c828d0a8165915213d6230019fe))
* **deps:** update module golang.org/x/text to v0.40.0 ([#225](https://github.com/fclairamb/dbbat/issues/225)) ([abf43ff](https://github.com/fclairamb/dbbat/commit/abf43ffcb91d1734123973855892f3f562f7fab6))

## [0.13.0](https://github.com/fclairamb/dbbat/compare/v0.12.0...v0.13.0) (2026-07-05)


### Features

* add Gateway API HTTPRoute support to the Helm chart ([#218](https://github.com/fclairamb/dbbat/issues/218)) ([1de8a62](https://github.com/fclairamb/dbbat/commit/1de8a627393779c075e17c2cf82241d8f902dccc))
* **oracle:** modern client support — sqlplus/OCI auth, SQLcl result capture, verifier-18453 ([#205](https://github.com/fclairamb/dbbat/issues/205)) ([a9858f6](https://github.com/fclairamb/dbbat/commit/a9858f6ccca16f26755fe126e961b816261cea6e))
* **oracle:** type-aware row & value capture with describe-record parser and bind extraction ([#195](https://github.com/fclairamb/dbbat/issues/195)) ([933a91e](https://github.com/fclairamb/dbbat/commit/933a91e5611adb985e82d0f500014a5f6212e1ef))


### Bug Fixes

* **ci:** restore unified dbbat-proxy service (pg+oracle+mysql) ([#220](https://github.com/fclairamb/dbbat/issues/220)) ([7bd9aa0](https://github.com/fclairamb/dbbat/commit/7bd9aa0793e77d62e037109b13acc4b0dd53e52f))
* **deps:** update module github.com/jackc/pgx/v5 to v5.10.0 ([#190](https://github.com/fclairamb/dbbat/issues/190)) ([d6a86c5](https://github.com/fclairamb/dbbat/commit/d6a86c55e2d4c1f88ae0296770f6e99fedd8e5e0))
* **deps:** update module github.com/knadh/koanf/v2 to v2.3.5 ([#187](https://github.com/fclairamb/dbbat/issues/187)) ([88f286e](https://github.com/fclairamb/dbbat/commit/88f286e519085eb4c4ea163eafc3428e185ef584))
* **deps:** update module github.com/slack-go/slack to v0.24.0 ([#184](https://github.com/fclairamb/dbbat/issues/184)) ([c532e11](https://github.com/fclairamb/dbbat/commit/c532e11a744671e1b5ac08de52eb057c314375ce))
* **deps:** update module github.com/slack-go/slack to v0.25.0 ([#191](https://github.com/fclairamb/dbbat/issues/191)) ([b6d2b01](https://github.com/fclairamb/dbbat/commit/b6d2b01aeafd2f22288228babfbf878e8c34a37a))
* **deps:** update module github.com/slack-go/slack to v0.26.0 ([#200](https://github.com/fclairamb/dbbat/issues/200)) ([5d72fc1](https://github.com/fclairamb/dbbat/commit/5d72fc18e99232444d1dcb1d0197a1fbd8fa7370))
* **deps:** update module github.com/slack-go/slack to v0.27.0 ([#210](https://github.com/fclairamb/dbbat/issues/210)) ([294d902](https://github.com/fclairamb/dbbat/commit/294d9020c058be7f8fe108395018abfb4b285049))
* **deps:** update module github.com/urfave/cli/v3 to v3.10.0 ([#201](https://github.com/fclairamb/dbbat/issues/201)) ([02d077e](https://github.com/fclairamb/dbbat/commit/02d077eb60e61a03bde4abb82e12cb3109f068c7))
* **deps:** update module github.com/urfave/cli/v3 to v3.10.1 ([#211](https://github.com/fclairamb/dbbat/issues/211)) ([76d52b9](https://github.com/fclairamb/dbbat/commit/76d52b914a08eb696ca09d44c684f759c3f8a7f7))
* **deps:** update module github.com/urfave/cli/v3 to v3.9.1 ([#196](https://github.com/fclairamb/dbbat/issues/196)) ([78b61fa](https://github.com/fclairamb/dbbat/commit/78b61fa5837953614bdf583c1e7f240e25b91ddd))
* **deps:** update module golang.org/x/crypto to v0.52.0 ([#180](https://github.com/fclairamb/dbbat/issues/180)) ([eadd0ba](https://github.com/fclairamb/dbbat/commit/eadd0bab2b5e8f27ad6909ba51e398f4580c48be))
* **deps:** update module golang.org/x/crypto to v0.53.0 ([#194](https://github.com/fclairamb/dbbat/issues/194)) ([0566f88](https://github.com/fclairamb/dbbat/commit/0566f88dc61a170ffe2396f885b29e6f69cc58e9))
* **deps:** update testcontainers-go monorepo to v0.43.0 ([#203](https://github.com/fclairamb/dbbat/issues/203)) ([6d9e06f](https://github.com/fclairamb/dbbat/commit/6d9e06fc47d43ee0fda06f818b6e80230dc861c7))

## [0.12.0](https://github.com/fclairamb/dbbat/compare/v0.11.0...v0.12.0) (2026-05-15)


### Features

* **ui:** add favicon from logo-notext.png ([#171](https://github.com/fclairamb/dbbat/issues/171)) ([35d82ba](https://github.com/fclairamb/dbbat/commit/35d82baea8dcb7fb15392c9fd21f21bf8aa277f3))


### Bug Fixes

* **dev:** fix dev mode routing so DBB_REDIRECTS proxy works without built frontend ([#173](https://github.com/fclairamb/dbbat/issues/173)) ([cb945d5](https://github.com/fclairamb/dbbat/commit/cb945d50c08097e049456467e011e57c8e1ff875))
* **ui:** fix favicon path and point preview at port 4200 ([#175](https://github.com/fclairamb/dbbat/issues/175)) ([863d123](https://github.com/fclairamb/dbbat/commit/863d123d38142eba2979f01d12fe339b0914a3b4))

## [0.11.0](https://github.com/fclairamb/dbbat/compare/v0.10.1...v0.11.0) (2026-05-15)


### Features

* **db:** add listable flag to databases ([#166](https://github.com/fclairamb/dbbat/issues/166)) ([c23c179](https://github.com/fclairamb/dbbat/commit/c23c17945ad2d03dac5004692b3f8f5ff847b9b4))
* grant definitions, grant requests, Slack notifications, global settings, connection URLs, auth fixes ([#168](https://github.com/fclairamb/dbbat/issues/168)) ([5136fc6](https://github.com/fclairamb/dbbat/commit/5136fc6833d0c9be5812249c395513096073e005))


### Bug Fixes

* **deps:** update module github.com/urfave/cli/v3 to v3.9.0 ([#163](https://github.com/fclairamb/dbbat/issues/163)) ([3a320b2](https://github.com/fclairamb/dbbat/commit/3a320b25ab2960bf02564927769c58bb3243d59b))
* **ui:** avoid setState-in-effect in PublicAdvertisementSection ([#169](https://github.com/fclairamb/dbbat/issues/169)) ([7c036fb](https://github.com/fclairamb/dbbat/commit/7c036fb9ad8591d14e89737fcc2a4376e1ac14d9))

## [0.10.1](https://github.com/fclairamb/dbbat/compare/v0.10.0...v0.10.1) (2026-05-11)


### Bug Fixes

* **auth:** re-creation of Slack-authenticated users after deletion ([#161](https://github.com/fclairamb/dbbat/issues/161)) ([a68af3a](https://github.com/fclairamb/dbbat/commit/a68af3ac01df0b1ca46d8d5a6abec4ca051ed77f))

## [0.10.0](https://github.com/fclairamb/dbbat/compare/v0.9.0...v0.10.0) (2026-05-10)


### Features

* **grants:** grant request workflow with Slack notifications and auto-refresh ([#157](https://github.com/fclairamb/dbbat/issues/157)) ([743fe20](https://github.com/fclairamb/dbbat/commit/743fe20201f96fc49173210431d54a6b5e68ee0b))
* **proxy:** PostgreSQL upstream TLS and SCRAM-SHA-256 auth ([#154](https://github.com/fclairamb/dbbat/issues/154)) ([196d5cc](https://github.com/fclairamb/dbbat/commit/196d5cc277882646e9628a4b68157078f3a58afb))


### Bug Fixes

* **config:** default Slack notify channel to #dbbat ([#159](https://github.com/fclairamb/dbbat/issues/159)) ([d47eba2](https://github.com/fclairamb/dbbat/commit/d47eba2fa9e1013923f49206c3f0dd08ec56b9a1))
* **deps:** update module github.com/knadh/koanf/parsers/toml/v2 to v2.2.1 ([#158](https://github.com/fclairamb/dbbat/issues/158)) ([616a10f](https://github.com/fclairamb/dbbat/commit/616a10f4de5a34c70bcac33210eeef5abacaf5b6))
* **deps:** update module github.com/slack-go/slack to v0.23.1 ([#160](https://github.com/fclairamb/dbbat/issues/160)) ([7d732e2](https://github.com/fclairamb/dbbat/commit/7d732e29149113a0cb6d5d1995d4235bb7128cdb))
* **deps:** update module golang.org/x/crypto to v0.51.0 ([#155](https://github.com/fclairamb/dbbat/issues/155)) ([7e04182](https://github.com/fclairamb/dbbat/commit/7e04182a16b3498ccc00af0a612056508c67dc4b))
* **deps:** update module golang.org/x/text to v0.37.0 ([#152](https://github.com/fclairamb/dbbat/issues/152)) ([58b3265](https://github.com/fclairamb/dbbat/commit/58b32655ac50c007c381f17a10fff79a51d77aa6))
* **grants:** populate query_count and bytes_transferred + UI polish ([#156](https://github.com/fclairamb/dbbat/issues/156)) ([e63de8c](https://github.com/fclairamb/dbbat/commit/e63de8cb0cc0fe185615b3e7d6e01d4abac88562))

## [0.9.0](https://github.com/fclairamb/dbbat/compare/v0.8.0...v0.9.0) (2026-05-08)


### Features

* **oracle:** customHash mode in O5LOGON server ([#143](https://github.com/fclairamb/dbbat/issues/143)) ([ff1a700](https://github.com/fclairamb/dbbat/commit/ff1a700429461ae7887f17d4da7fd4f8c6c2b465))
* **oracle:** derive combined key for empty-AUTH_PASSWORD path ([#148](https://github.com/fclairamb/dbbat/issues/148)) ([ce4bb2c](https://github.com/fclairamb/dbbat/commit/ce4bb2c709134256c50a2342f120e0fb9f50960b))
* **oracle:** forward client's actual Phase 1 to upstream ([#138](https://github.com/fclairamb/dbbat/issues/138)) ([126caa1](https://github.com/fclairamb/dbbat/commit/126caa18bc1f52fa7ac9b2f43f71d88f5f0f1da6))
* **oracle:** forward client's actual Phase 2 to upstream ([#144](https://github.com/fclairamb/dbbat/issues/144)) ([ebc9df1](https://github.com/fclairamb/dbbat/commit/ebc9df1aa3bdb5faf17a7ae29f6ba9389dbc8baf))


### Bug Fixes

* **oracle:** trim AUTH challenge end-marker to 33 bytes (SQLcl unblocks) ([#150](https://github.com/fclairamb/dbbat/issues/150)) ([189f011](https://github.com/fclairamb/dbbat/commit/189f0117883c178126ba15af20d544f2121d41f5))
* **oracle:** unbreak upstream auth parser; patch AUTH_SVR_RESPONSE ([#136](https://github.com/fclairamb/dbbat/issues/136)) ([db28eb6](https://github.com/fclairamb/dbbat/commit/db28eb673d5fb8e54bf6540b015ceb29436887a5))

## [0.8.0](https://github.com/fclairamb/dbbat/compare/v0.7.0...v0.8.0) (2026-05-06)


### Features

* **proxy:** PostgreSQL TLS termination ([#131](https://github.com/fclairamb/dbbat/issues/131)) ([8c76c00](https://github.com/fclairamb/dbbat/commit/8c76c00530244dd6fb50f98b7d9c324747e223a5))


### Bug Fixes

* **api:** fold accents in Slack OAuth username generation ([#130](https://github.com/fclairamb/dbbat/issues/130)) ([08b7fd7](https://github.com/fclairamb/dbbat/commit/08b7fd7b89fd0b853433574c52893e9a5861c19a))
* **oracle:** use user_id_len for structured Phase 1 username parsing ([#134](https://github.com/fclairamb/dbbat/issues/134)) ([5593564](https://github.com/fclairamb/dbbat/commit/5593564e81e274cfa7a9674722967459a4f695db))

## [0.7.0](https://github.com/fclairamb/dbbat/compare/v0.6.0...v0.7.0) (2026-05-06)


### Features

* **proxy:** add MySQL/MariaDB proxy with caching_sha2_password and TLS ([#112](https://github.com/fclairamb/dbbat/issues/112)) ([b916818](https://github.com/fclairamb/dbbat/commit/b916818e9ae3eec205d32db62d016265599b2a0f))
* **proxy:** harden MySQL upstream against LOCAL INFILE + verify binary row capture ([#115](https://github.com/fclairamb/dbbat/issues/115)) ([4a17b6f](https://github.com/fclairamb/dbbat/commit/4a17b6f135959d60ce1edb645e3bc31e4b2c0406))
* **proxy:** Oracle terminated auth — go-ora end-to-end working ([#118](https://github.com/fclairamb/dbbat/issues/118)) ([3a27833](https://github.com/fclairamb/dbbat/commit/3a278333936aadbc8180fc8d5d52cd443c1ff90f))
* **proxy:** wire up MySQL session packet dumps ([#116](https://github.com/fclairamb/dbbat/issues/116)) ([f7a81b8](https://github.com/fclairamb/dbbat/commit/f7a81b87b9693aef8caa7a5b3ec342ef93502a5f))


### Bug Fixes

* **deps:** update docusaurus monorepo to v3.10.1 ([#122](https://github.com/fclairamb/dbbat/issues/122)) ([102bd08](https://github.com/fclairamb/dbbat/commit/102bd08f83e099b7204482f30f23e578a210dec1))
* **deps:** update module github.com/go-mysql-org/go-mysql to v1.15.0 ([#128](https://github.com/fclairamb/dbbat/issues/128)) ([ad6b2a3](https://github.com/fclairamb/dbbat/commit/ad6b2a338fbb6d56d40969561c8427dd55a2649b))
* **deps:** update module github.com/go-sql-driver/mysql to v1.10.0 ([#132](https://github.com/fclairamb/dbbat/issues/132)) ([14be368](https://github.com/fclairamb/dbbat/commit/14be3682a8fed488fe4d5994a63e15068f179298))
* **deps:** update module github.com/jackc/pgx/v5 to v5.9.2 ([#108](https://github.com/fclairamb/dbbat/issues/108)) ([b837285](https://github.com/fclairamb/dbbat/commit/b837285f589cba224177cbd51fe523e31d995ec1))
* **proxy:** keep relay socket through AUTH so SQLcl avoids ORA-03120 ([#129](https://github.com/fclairamb/dbbat/issues/129)) ([a23c060](https://github.com/fclairamb/dbbat/commit/a23c0607bfbcd188c402e47e7499809d52c4feca))

## [0.6.0](https://github.com/fclairamb/dbbat/compare/v0.5.2...v0.6.0) (2026-04-15)


### Features

* **proxy:** activate Oracle terminated auth with O5LOGON and API keys ([#105](https://github.com/fclairamb/dbbat/issues/105)) ([d90d64e](https://github.com/fclairamb/dbbat/commit/d90d64e9da0a88907cbeb1b6dd2f5a9fdbf55395))

## [0.5.2](https://github.com/fclairamb/dbbat/compare/v0.5.1...v0.5.2) (2026-04-14)


### Bug Fixes

* **auth:** redirect OAuth callback to /login route instead of root ([#103](https://github.com/fclairamb/dbbat/issues/103)) ([842b5ba](https://github.com/fclairamb/dbbat/commit/842b5ba1bb9484f60949057cb249a08a360617ba))

## [0.5.1](https://github.com/fclairamb/dbbat/compare/v0.5.0...v0.5.1) (2026-04-14)


### Bug Fixes

* **config:** remove redundant slack_ prefix from SlackAuthConfig koanf tags ([#101](https://github.com/fclairamb/dbbat/issues/101)) ([e3882cd](https://github.com/fclairamb/dbbat/commit/e3882cd926b93378ea61acd2eb401a35e62354fa))

## [0.5.0](https://github.com/fclairamb/dbbat/compare/v0.4.0...v0.5.0) (2026-04-14)


### Features

* **proxy:** API key auth for PG proxy, Oracle auth, and Oracle SERVICE_NAME handling rewrite ([#79](https://github.com/fclairamb/dbbat/issues/79)) ([83858a1](https://github.com/fclairamb/dbbat/commit/83858a136648ca866ef4906fbc79c170ae2dfa4b))


### Bug Fixes

* **deps:** update dependency zod to v4 ([#91](https://github.com/fclairamb/dbbat/issues/91)) ([c92061a](https://github.com/fclairamb/dbbat/commit/c92061a03fe38e373f9d5199435c5f3f8e87b864))
* **deps:** update docusaurus monorepo to v3.10.0 ([#89](https://github.com/fclairamb/dbbat/issues/89)) ([a6c417c](https://github.com/fclairamb/dbbat/commit/a6c417c9fdffcaa5dd59f1c8beceefe6ded3a0d2))
* **deps:** update module golang.org/x/crypto to v0.50.0 ([#93](https://github.com/fclairamb/dbbat/issues/93)) ([13a91fa](https://github.com/fclairamb/dbbat/commit/13a91fade65ee4ea68719980a39bb9eae458b148))
* **deps:** update testcontainers-go monorepo to v0.41.0 ([#88](https://github.com/fclairamb/dbbat/issues/88)) ([79fecd7](https://github.com/fclairamb/dbbat/commit/79fecd78ee8a2c0498f5275597a687feedb1d159))
* **deps:** update testcontainers-go monorepo to v0.42.0 ([#94](https://github.com/fclairamb/dbbat/issues/94)) ([9977e30](https://github.com/fclairamb/dbbat/commit/9977e30b9a60afd6a6e2745de0bd285b921ec9a5))

## [0.4.0](https://github.com/fclairamb/dbbat/compare/v0.3.0...v0.4.0) (2026-04-04)


### Features

* **proxy:** Add a first draft of Oracle support ([#75](https://github.com/fclairamb/dbbat/issues/75)) ([908abbb](https://github.com/fclairamb/dbbat/commit/908abbbca1c87d1aa3cf82cad9d0088df52c9df0))
* **proxy:** Oracle session TNS dump capture ([#78](https://github.com/fclairamb/dbbat/issues/78)) ([7e0b45a](https://github.com/fclairamb/dbbat/commit/7e0b45a4e1d86754b93b6a59779925a998027896))


### Bug Fixes

* **ci:** autoupdate ([#57](https://github.com/fclairamb/dbbat/issues/57)) ([bb9b9ad](https://github.com/fclairamb/dbbat/commit/bb9b9ad913fbaab67750a9d44783b24106bed175))
* **deps:** update dependency openapi-fetch to ^0.16.0 ([#68](https://github.com/fclairamb/dbbat/issues/68)) ([567412d](https://github.com/fclairamb/dbbat/commit/567412dca3ea95ad2ea2cfca6e70004e18cccc6f))
* **deps:** update module github.com/knadh/koanf/v2 to v2.3.2 ([#59](https://github.com/fclairamb/dbbat/issues/59)) ([9b57552](https://github.com/fclairamb/dbbat/commit/9b575525abe7887f3f524c46e28d10573d4269a5))
* **deps:** update module golang.org/x/crypto to v0.48.0 ([#69](https://github.com/fclairamb/dbbat/issues/69)) ([ff7856b](https://github.com/fclairamb/dbbat/commit/ff7856b26c426b6dfbba678f2327ef34c67d8e9a))
* resolve test races, E2E error format, and lint issues ([#77](https://github.com/fclairamb/dbbat/issues/77)) ([c95b303](https://github.com/fclairamb/dbbat/commit/c95b303bbed9936509100932dc017a73b25678c6))

## [0.3.0](https://github.com/fclairamb/dbbat/compare/v0.2.0...v0.3.0) (2026-01-24)


### Features

* **api:** add admin password reset endpoint ([#51](https://github.com/fclairamb/dbbat/issues/51)) ([529fc92](https://github.com/fclairamb/dbbat/commit/529fc92da65b8e1ffe831d06530a93493b4c4602))
* **ui:** add quota fields to grant creation form ([#49](https://github.com/fclairamb/dbbat/issues/49)) ([626fa90](https://github.com/fclairamb/dbbat/commit/626fa90e74ac4de7d4be0970a63cf0fc1ffbc234))


### Bug Fixes

* **deps:** update dependency lucide-react to ^0.563.0 ([#48](https://github.com/fclairamb/dbbat/issues/48)) ([e7ca0b6](https://github.com/fclairamb/dbbat/commit/e7ca0b664b943bc1489f9685c0984f9a783adbe4))
* **deps:** update module github.com/knadh/koanf/v2 to v2.3.1 ([#52](https://github.com/fclairamb/dbbat/issues/52)) ([8b26ec9](https://github.com/fclairamb/dbbat/commit/8b26ec9259873c239c611ba4f1cffc948f524bd8))
* **deps:** update module github.com/urfave/cli/v3 to v3.6.2 ([#46](https://github.com/fclairamb/dbbat/issues/46)) ([32684fe](https://github.com/fclairamb/dbbat/commit/32684fed5d6e94b04c7715c104f08a4bfde66f8a))
* reduce argon2id memory and protect admin in demo mode ([#50](https://github.com/fclairamb/dbbat/issues/50)) ([fc90f6d](https://github.com/fclairamb/dbbat/commit/fc90f6d34581042e22bedc6d846c8bf31299fe64))
* **test:** remove flaky toBeDisabled assertions in E2E tests ([#54](https://github.com/fclairamb/dbbat/issues/54)) ([2dc8e1a](https://github.com/fclairamb/dbbat/commit/2dc8e1a4d01bcd396bba4cd01923488a7060e81f))

## [0.2.0](https://github.com/fclairamb/dbbat/compare/v0.1.0...v0.2.0) (2026-01-13)


### Features

* **ui:** add time precision to grant date inputs ([#39](https://github.com/fclairamb/dbbat/issues/39)) ([e465e2c](https://github.com/fclairamb/dbbat/commit/e465e2cd53ce75ff817f74cc5e4b061dd68d8d8b))


### Bug Fixes

* **deps:** update module github.com/knadh/koanf/parsers/toml to v2 ([#35](https://github.com/fclairamb/dbbat/issues/35)) ([7c56f99](https://github.com/fclairamb/dbbat/commit/7c56f9932f87309259f29f5a66a72eeb4f255cf5))
* **deps:** update module github.com/knadh/koanf/parsers/toml to v2 ([#37](https://github.com/fclairamb/dbbat/issues/37)) ([767bcc8](https://github.com/fclairamb/dbbat/commit/767bcc8dcd9e3271d1229ec3053f3af19b770925))
* **deps:** update module github.com/knadh/koanf/providers/env to v2 ([#38](https://github.com/fclairamb/dbbat/issues/38)) ([bfe821c](https://github.com/fclairamb/dbbat/commit/bfe821cc0db20bf3692b1f3f0eca2c677709ed21))

## [0.1.0](https://github.com/fclairamb/dbbat/compare/v0.0.2...v0.1.0) (2026-01-12)


### Features

* **config:** add configurable log level with strict sloglint compliance ([#31](https://github.com/fclairamb/dbbat/issues/31)) ([51fa451](https://github.com/fclairamb/dbbat/commit/51fa451df2e90150e59dd1dad586c9bfd70998af))


### Bug Fixes

* **deps:** update module golang.org/x/crypto to v0.47.0 ([#34](https://github.com/fclairamb/dbbat/issues/34)) ([c18a454](https://github.com/fclairamb/dbbat/commit/c18a454c6b29f2a371cadc478a4d51601b479c79))


### Performance Improvements

* **auth:** extend AuthCache to API key and web session verification ([#32](https://github.com/fclairamb/dbbat/issues/32)) ([fa21f84](https://github.com/fclairamb/dbbat/commit/fa21f846f404fe8161abf5620c0fc0bfb655de56))

## [0.0.2](https://github.com/fclairamb/dbbat/compare/v0.0.1...v0.0.2) (2026-01-11)


### Bug Fixes

* use PAT for release-please to trigger release workflow ([#28](https://github.com/fclairamb/dbbat/issues/28)) ([1566de4](https://github.com/fclairamb/dbbat/commit/1566de4c2bd26628ef6303ce853f4163fce0f2a9))

## [0.0.1](https://github.com/fclairamb/dbbat/compare/v0.0.0...v0.0.1) (2026-01-11)

### Bug Fixes

* **ui:** handle 401 errors by redirecting to login page ([#24](https://github.com/fclairamb/dbbat/issues/24)) ([b8d205c](https://github.com/fclairamb/dbbat/commit/b8d205c8d08eaf890ff885bf28f34e46baf76d5c))

### Performance Improvements

* **auth:** implement password verification cache and configurable hash parameters ([#25](https://github.com/fclairamb/dbbat/issues/25)) ([ea0dd0b](https://github.com/fclairamb/dbbat/commit/ea0dd0b2ccc139f650e1908da0a532e0d7af63e4)), closes [#22](https://github.com/fclairamb/dbbat/issues/22)
