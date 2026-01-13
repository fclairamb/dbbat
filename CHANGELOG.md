# Changelog

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
