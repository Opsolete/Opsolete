# Changelog

## [0.3.0](https://github.com/Opsolete/Opsolete/compare/v0.2.0...v0.3.0) (2026-10-29)


### Features

* expose Prometheus metrics on a separate METRICS_ADDR listener ([65095f3](https://github.com/Opsolete/Opsolete/commit/65095f34e52cb14a1967b967cb79abca1d632fd5))

## [0.2.0](https://github.com/Opsolete/Opsolete/compare/v0.1.0...v0.2.0) (2026-10-28)


### ⚠ BREAKING CHANGES

* module uploads are off until an admin enables them in Settings → Organization; TEMPLATE_EDITOR=1 no longer enables them outside development. The Nix package and container images no longer ship catalog/: configure a Git repository in Settings → Templates, or point TEMPLATES_DIR at a folder of templates.

### Features

* load and edit the template catalog from a Git repository ([10138e4](https://github.com/Opsolete/Opsolete/commit/10138e4678126a0debd3dcf2e2d2c03fd8156a9f))


### Bug Fixes

* clarify the catalog message shown when no repository is connected ([fabaf21](https://github.com/Opsolete/Opsolete/commit/fabaf2179cbf2dc0d1dd9bb5ff506c23567c4853))
* fence state locks and writes, and harden runs, VCS polling and login ([ff3da68](https://github.com/Opsolete/Opsolete/commit/ff3da68a67cf57b4e7dbeb3385fa13595c813c96))
* guard catalog syncs against stale writes and publish release images ([36bb4c1](https://github.com/Opsolete/Opsolete/commit/36bb4c11bac56029094f02ee134081ccd023c686))
* keep catalog sync status accurate under stale syncs and failures ([66ff1b4](https://github.com/Opsolete/Opsolete/commit/66ff1b420eb1bf307d1febb5f376b6d4f89caa8a))
* key users by OIDC subject and harden config, runner and template checks ([49095ec](https://github.com/Opsolete/Opsolete/commit/49095ec7120eeaeb9569c040d7a166a0c77a938e))

## 0.1.0 (2026-10-28)


### ⚠ BREAKING CHANGES

* **runner:** authenticate runners with a shared RUNNER_TOKEN
* SECRET_KEY is now required. The server and `opsolete token` refuse to start without it (at least 32 characters).

### Features

* add new styling wizards and app frame ([5a949ac](https://github.com/Opsolete/Opsolete/commit/5a949ace0170b03656be088a09fafb932fc9d310))
* add organization setting for cost estimate currency ([284255a](https://github.com/Opsolete/Opsolete/commit/284255ace6fe92e125c8574bc03d3f7476c8d5ea))
* add state version page with diffs ([d63b14c](https://github.com/Opsolete/Opsolete/commit/d63b14c7d9769f7d07759dc7a3771ea7d406b13f))
* add tag-based VCS triggers and a shared workspace picker ([582cab8](https://github.com/Opsolete/Opsolete/commit/582cab82c6a3d23cc17e82b2b05fb703d9ac3dca))
* add tamper-evident audit log ([a4ae345](https://github.com/Opsolete/Opsolete/commit/a4ae345891ad764439a9fcf0e52c570e9817280f))
* add workspace locking and plan-only runs ([f9d7a3b](https://github.com/Opsolete/Opsolete/commit/f9d7a3b08984ebf5e287caf0c8a95f169c936e0f))
* **audit:** split changes from (failed) sign-ins with a tab ([d8dfefc](https://github.com/Opsolete/Opsolete/commit/d8dfefceb1d76f84483285ab57e400431a2b6182))
* build template forms from the pinned module's variables ([9c9b9eb](https://github.com/Opsolete/Opsolete/commit/9c9b9eb216f2b8726522d89c948115fe5b297735))
* estimate monthly cost of plans with Infracost ([505c3ce](https://github.com/Opsolete/Opsolete/commit/505c3cecc417382cec9cc477a34fed07e43896a1))
* **nix:** add NixOS module for server and runner ([8434f22](https://github.com/Opsolete/Opsolete/commit/8434f22f410463c2063d2bf80656e6c02d5e3dad))
* **runner:** authenticate runners with a shared RUNNER_TOKEN ([d58d5d3](https://github.com/Opsolete/Opsolete/commit/d58d5d300c6d3d3751ebe922038c1c49397b8802))
* scope VCS connections by workspace instead of repo pattern ([c26e692](https://github.com/Opsolete/Opsolete/commit/c26e6923b81f80998f97cf75fab888c28ab20a60))


### Bug Fixes

* harden state, secrets and VCS triggers; speed up pages and queries ([e388787](https://github.com/Opsolete/Opsolete/commit/e38878762c8b55d271fccf5d3fbfcaa1d4aef864))
* only show if changes are present ([9d154e0](https://github.com/Opsolete/Opsolete/commit/9d154e04a0b76e1c13cfc98c581c0fca050be262))
* release orphaned state locks ([f3ca74e](https://github.com/Opsolete/Opsolete/commit/f3ca74e167905db1104c601f1062632712ad382e))
* **runner:** capture token only ([579f758](https://github.com/Opsolete/Opsolete/commit/579f758732fadf5e1e4e288aab0f7b71f8bf26dd))
