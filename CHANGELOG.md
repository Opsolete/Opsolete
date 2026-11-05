# Changelog

## [0.5.1](https://github.com/Opsolete/Opsolete/compare/v0.5.0...v0.5.1) (2026-11-05)


### Bug Fixes

* **flake:** update vendorHash and group Dependabot Go updates ([543e396](https://github.com/Opsolete/Opsolete/commit/543e3964fa5b9cdb7c8a382adf7694cc22182bae))

## [0.5.0](https://github.com/Opsolete/Opsolete/compare/v0.4.0...v0.5.0) (2026-11-04)


### ⚠ BREAKING CHANGES

* **templates:** show_if may only refer to an earlier select input; conditions on bool inputs and on individual select options are removed.

### Features

* **audit:** one-line summaries with full detail on click, redact secret-named fields ([004bb34](https://github.com/Opsolete/Opsolete/commit/004bb34b88fb7b9d2aed23474addbf60f58c5a77))
* **builder:** pick the key field in one "Workspace name from" dropdown ([8544753](https://github.com/Opsolete/Opsolete/commit/854475341472517f3eac943387a3c0995fadda47))
* **templates:** edit dropdown options as value / shown-as rows in the builder ([cae526f](https://github.com/Opsolete/Opsolete/commit/cae526f4a7837a0a72ce626997c31c0a17326756))
* **templates:** edit show-when conditions in a reworked template builder ([1edaf84](https://github.com/Opsolete/Opsolete/commit/1edaf840b7cff6748ded2f24f6311c91b4470ec3))
* **templates:** key input names a template added to a workspace ([00c8a8e](https://github.com/Opsolete/Opsolete/commit/00c8a8ea4a18732b664ecf9ad631101ff8e0d18c))
* **workspaces:** Deploy… page to add templates from the catalog ([0909977](https://github.com/Opsolete/Opsolete/commit/090997796a2c2fa97efe9560c29b2a60bbc89004))
* **workspaces:** force delete a workspace whose state still lists resources ([8cbd288](https://github.com/Opsolete/Opsolete/commit/8cbd288e95835e95c406555b75bb2192123879ad))
* **workspaces:** show per template whether it has resources in state ([04a2601](https://github.com/Opsolete/Opsolete/commit/04a2601c94506c30d0054525ba85245884d08fc5))


### Bug Fixes

* **audit:** record the escaped path so odd paths are still audited, show the peer address next to X-Forwarded-For ([76c81fd](https://github.com/Opsolete/Opsolete/commit/76c81fda7ca5502f74483575580a2e077eeaba27))
* **auth:** take the admin role from ADMIN_EMAILS on every request, not from the session or the database ([748a620](https://github.com/Opsolete/Opsolete/commit/748a6200afe762d64c98ecf4f7b0bc5404db566f))
* **cloud:** JSON:API error objects for older go-tfe, refuse state uploads to remote workspaces, cap decoded state size ([b789d7c](https://github.com/Opsolete/Opsolete/commit/b789d7c5a8aa2bf20791654e96b2c8484c0a7da3))
* **flake:** update vendorHash ([9aa0dc6](https://github.com/Opsolete/Opsolete/commit/9aa0dc6aa5f3d0ee92bc29862f4de429da50c803))
* **nix:** give a stopping runner's job 5 minutes past its timeout to save state ([3e4030d](https://github.com/Opsolete/Opsolete/commit/3e4030dbee0864f7c9d8b334d3130da6fdc0ad87))
* **nix:** the runner reads its secrets as root and drops to its user, so job code can't read the runner token ([6d37990](https://github.com/Opsolete/Opsolete/commit/6d379905a88ad5869a4742b06fd88a17344b7b63))
* **nix:** write file state store temp files next to the target ([4a903dd](https://github.com/Opsolete/Opsolete/commit/4a903ddeac2692f2a17c9c77c69ce82c7c1e2478))
* **registry:** match this registry's module sources case-insensitively, drop an unreachable extract check ([5a28275](https://github.com/Opsolete/Opsolete/commit/5a28275634f233fe92811d5db0b92e6f1075794f))
* **runner:** keep the state token off the command line and saved plan, redact messages before truncating ([d4429a2](https://github.com/Opsolete/Opsolete/commit/d4429a2974d65f2a31502dc200fae586f0a8c2f8))
* **server:** wait for in-flight requests before closing the store on shutdown ([fe66467](https://github.com/Opsolete/Opsolete/commit/fe66467b1da126be1dd3970b65c034de2e75f7a3))
* **state:** pull request plans can't read other workspaces' shared state ([7b00908](https://github.com/Opsolete/Opsolete/commit/7b00908c3d5bed2ca1238ecac49feee24bcf7992))
* **storage:** concurrent first logins, empty lock holder after a lost lock race, read a created workspace by id ([6a01b7f](https://github.com/Opsolete/Opsolete/commit/6a01b7fbf205e8c249993e566199ce1fe766e7cd))
* **templates:** say how to fix a refused VCS token when saving or deleting a template ([01e7114](https://github.com/Opsolete/Opsolete/commit/01e7114eb148c16e260eba5317f77b9d0cf60858))
* **vcs:** enable VCS connections with deployments too, publish modules only with the registry ([9b07b63](https://github.com/Opsolete/Opsolete/commit/9b07b63f833421afce974178f70073c3b69e4e96))
* **vcs:** rank a prerelease tag below its release, don't load a stale catalog snapshot first ([924fc21](https://github.com/Opsolete/Opsolete/commit/924fc2141ea3d4bc718bb4922e3be062ea1a8746))
* **workspaces:** delete unindexed workspaces from the UI, validate settings before saving, keep VCS scope on create, finish cleanups after disconnects ([fa96863](https://github.com/Opsolete/Opsolete/commit/fa9686385e06fa596ad1cca3664b13eb610235be))
* **workspaces:** name an added template from its key plus a random hex suffix, drop the name field ([09ab4f8](https://github.com/Opsolete/Opsolete/commit/09ab4f8f7097ceaa2e23fb552127b3737b2598e9))


### Performance Improvements

* **storage:** index status history, rollback links and workspace cascades, drop an unused index ([35c6f35](https://github.com/Opsolete/Opsolete/commit/35c6f352c6c23bd9d002cf5f5919e8ae5b6cd12b))

## [0.4.0](https://github.com/Opsolete/Opsolete/compare/v0.3.0...v0.4.0) (2026-11-03)


### Features

* delete templates from the catalog ([a84ed4b](https://github.com/Opsolete/Opsolete/commit/a84ed4bca84f40e81b1fb771088eadc8919479a6))
* **templates:** conditional inputs and labelled select options ([86e22a8](https://github.com/Opsolete/Opsolete/commit/86e22a85e2af8e28612506d3b9d4478115033a6e))
* **templates:** sensitive template inputs ([36ec2e5](https://github.com/Opsolete/Opsolete/commit/36ec2e547d1c3c57e2db5f859ca27f1b5803a4ce))
* **workspaces:** group resources by module and rename Replace to Recreate ([92f010e](https://github.com/Opsolete/Opsolete/commit/92f010e47b5ce20348bd799ac0772b4f510d10e2))
* **workspaces:** show template names and collapse inputs on the templates tab ([f9678b7](https://github.com/Opsolete/Opsolete/commit/f9678b7d5940d2270a6dae0d369e607233b8b41e))


### Bug Fixes

* **catalog:** reset repository, branch and folder when switching VCS connection ([f949770](https://github.com/Opsolete/Opsolete/commit/f949770d99b84e7e7844f20b02b496011068b1c7))
* **registry:** skip monorepo tags from before a module's folder existed ([3af513d](https://github.com/Opsolete/Opsolete/commit/3af513d4782fe8cda3f0d58a7ebe0a22b68b6859))
* **workspaces:** destroy a removed template's resources with -target ([b99259d](https://github.com/Opsolete/Opsolete/commit/b99259d2f682ac733dcecf8337d537678247d9e3))

## [0.3.0](https://github.com/Opsolete/Opsolete/compare/v0.2.0...v0.3.0) (2026-11-03)


### Features

* change the template catalog's access token and fix the token permission text ([a3fde68](https://github.com/Opsolete/Opsolete/commit/a3fde6864501906d8b21abf60acf092243538d1d))
* expose Prometheus metrics on a separate METRICS_ADDR listener ([65095f3](https://github.com/Opsolete/Opsolete/commit/65095f34e52cb14a1967b967cb79abca1d632fd5))
* publish VCS modules from a folder and unlink them from the registry ([aa42db3](https://github.com/Opsolete/Opsolete/commit/aa42db34cfa50c4cd860a99cf26b496674d3fdc5))
* treat VCS connections as shared credentials for workspaces, modules and templates ([7026a0d](https://github.com/Opsolete/Opsolete/commit/7026a0d99ef981688f0f6c4d52ca98f624e6689a))


### Bug Fixes

* **image:** write runner passwd/group as plain files ([8b21518](https://github.com/Opsolete/Opsolete/commit/8b215180b26a19365f25e7f784715bbd2ff9d5dd))

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
