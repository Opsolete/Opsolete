# Changelog

## [0.10.0](https://github.com/Opsolete/Opsolete/compare/v0.9.0...v0.10.0) (2026-11-14)


### ⚠ BREAKING CHANGES

* **api:** /api/v1/workspaces/{name}/... is /api/v1/workspaces/{id}/...; creating a workspace takes project_id, not project; the workspace JSON has project_id and drift_detection.
* **ui:** the Templates tab (?show=templates) is gone; any other show= value shows the Resources tab.

### Features

* **api:** platform objects by id for a Terraform provider; service tokens ([46365a1](https://github.com/Opsolete/Opsolete/commit/46365a12e80c62748baf785c58bfa74880ebfc83))
* **ui:** a template card's title holds its version and an update tag; recreate inside the resource list ([eaebf1c](https://github.com/Opsolete/Opsolete/commit/eaebf1cd60caedea198b4b947280755a2ddf0933))
* **ui:** one Resources tab: templates are its groups, with Configure and Remove ([6529d7c](https://github.com/Opsolete/Opsolete/commit/6529d7ca2afaa86b01b8e0ea09c060dbb20dacd4))
* **ui:** template groups as cards: a readable header with its buttons, resources fold open ([ca4af9a](https://github.com/Opsolete/Opsolete/commit/ca4af9af64d3ed9252e654958a63f5b5ee466f3e))


### Bug Fixes

* **api:** a token sent wins over a session ([6fd80e1](https://github.com/Opsolete/Opsolete/commit/6fd80e1da5def161c18adcd42d2a214281810f1c))
* **api:** a token sent wins over a session ([2d8d860](https://github.com/Opsolete/Opsolete/commit/2d8d860028fdce5a3f679abb6cb9f15967fa87e1))
* **deps:** bump the go group with 3 updates ([884ced1](https://github.com/Opsolete/Opsolete/commit/884ced18e21aede69bd6d61509201498e7640420))
* **deps:** bump the go group with 3 updates ([b29e328](https://github.com/Opsolete/Opsolete/commit/b29e328e7f94d22be8795699da4673a11373dd54))
* **flake:** update vendorHash ([3f18dc9](https://github.com/Opsolete/Opsolete/commit/3f18dc987f2a4228b91abc838ea016d3586bca01))
* **templates:** another registry's module gets a version argument, not ?ref= ([d99577e](https://github.com/Opsolete/Opsolete/commit/d99577e49752b42c55601068848443e8ad8adde2))
* **templates:** another registry's module gets a version argument, not ?ref= ([2b32649](https://github.com/Opsolete/Opsolete/commit/2b32649b0e76eb498d2818714cbae20deaa34b8a))

## [0.9.0](https://github.com/Opsolete/Opsolete/compare/v0.8.0...v0.9.0) (2026-11-13)


### Features

* HCL Terraform variables; cloud API outputs with values for readers, workspace list ([5b96286](https://github.com/Opsolete/Opsolete/commit/5b9628600877c4bbe0ac50c5d3259a2bd0729aa7))
* **import:** check templates that import; stop importing from the form; clearer import texts ([809900d](https://github.com/Opsolete/Opsolete/commit/809900d5a8f1d81e33233bf1219cdb242c51b7e3))
* **runs:** a run queue: runs started while one is active wait as pending ([fd8bbe6](https://github.com/Opsolete/Opsolete/commit/fd8bbe660d5a518eb273336e69197a89e859b79d))
* **runs:** drift checks and pull request plans run beside the workspace's runs ([a0125cb](https://github.com/Opsolete/Opsolete/commit/a0125cb129e45ffc28365b868a2242ef66a35766))
* **runs:** drift checks and pull request plans run beside the workspace's runs ([8db704c](https://github.com/Opsolete/Opsolete/commit/8db704c9a7ed7c3383ac017c72fa7ddee56fde1b))
* **ui:** a workspace's resources start collapsed per module ([6fa2c8e](https://github.com/Opsolete/Opsolete/commit/6fa2c8eaf87441919ec3764fc22ed8ad52c38294))
* **ui:** a workspace's resources start collapsed per module ([8ff3a74](https://github.com/Opsolete/Opsolete/commit/8ff3a7460910b4aacec38a4c60fddb9bbd0429d9))
* **ui:** a workspace's templates as a plain table, without their inputs ([e4f507a](https://github.com/Opsolete/Opsolete/commit/e4f507a907bfa28ccb18d0fa623ba4ced836dc46))
* **ui:** a workspace's templates as a plain table, without their inputs ([fa4330c](https://github.com/Opsolete/Opsolete/commit/fa4330c21df4f622a4db8dc7a78b93c4adecd24e))
* **ui:** the registry page lists modules without a usage block ([76d683e](https://github.com/Opsolete/Opsolete/commit/76d683e8a09f0c63b6761a20034b2a74a7ff8895))
* **ui:** the registry page lists modules without a usage block ([2156ac2](https://github.com/Opsolete/Opsolete/commit/2156ac2c2abdb0689fd6d78b6044e83d0605ee37))
* **variables:** priority variable sets ([165d5f7](https://github.com/Opsolete/Opsolete/commit/165d5f7f0803dbc015de8fa95a9ca6dfb1bdbbf7))
* **variables:** Terraform variables fill the inputs a template doesn't list ([c9c57c7](https://github.com/Opsolete/Opsolete/commit/c9c57c72cbaee752f07dbed63a4b251e0da3e0a3))
* **variables:** Terraform variables fill the inputs a template doesn't list ([1ce0607](https://github.com/Opsolete/Opsolete/commit/1ce06077ff7c22acbf0e222a31c1ba9193a7e1dd))
* **vcs:** a push while a run is active waits as pending; only the newest push stays ([a3ca436](https://github.com/Opsolete/Opsolete/commit/a3ca4367ba29704333c8d41242d3576abbe60cdd))


### Bug Fixes

* harden import and templates; run queue, HCL and priority variables ([acbd46c](https://github.com/Opsolete/Opsolete/commit/acbd46cd37138bba19575b37f17e285c8e0c2294))
* **import:** a round shows the project's value as what it planned with ([4dac980](https://github.com/Opsolete/Opsolete/commit/4dac98066e1ee9c1f5f147060f9caaf6a2655ec3))
* **import:** a round shows the project's value as what it planned with ([daf37f5](https://github.com/Opsolete/Opsolete/commit/daf37f5e10b59edb76f060dec27d191fc44a697f))
* **import:** only pending rounds go on; a stricter one-state guard; retention keeps rounds ([a28bd68](https://github.com/Opsolete/Opsolete/commit/a28bd68ed9d2ee05b07e88ecfd38dfe1c9dea604))
* **runner:** a drift check isn't drift when only data sources were read again ([049a37e](https://github.com/Opsolete/Opsolete/commit/049a37efe8321f0bbe368d1643df7138b78e6004))
* **runner:** a drift check isn't drift when only data sources were read again ([bfedafa](https://github.com/Opsolete/Opsolete/commit/bfedafa4abfa254e429b784af697d41db44ceca2))
* **runner:** an output that isn't saved yet isn't drift ([5ffc95c](https://github.com/Opsolete/Opsolete/commit/5ffc95c72c37b5a2b602beacf3f9a9acb8412262))
* **state:** unlocking a lock someone else holds is an error, not OK ([6455eee](https://github.com/Opsolete/Opsolete/commit/6455eeecfe74a834ff750fcb67f6babe75a81c7c))
* **templates:** an import switch doesn't need a key input ([3ff388a](https://github.com/Opsolete/Opsolete/commit/3ff388abe4db6dae9565527e173b08942f71ebe7))
* **templates:** an import switch doesn't need a key input ([a937e39](https://github.com/Opsolete/Opsolete/commit/a937e3951df1eb8db8bdc18798e02c37b186e520))
* **templates:** check templates as users get them; rollouts say why; stale row selects shown ([33c0f54](https://github.com/Opsolete/Opsolete/commit/33c0f548da630f97c9f473d05cb5ffd63671b72b))
* **templates:** no silent deploy changes or plain secrets when templates change ([05e62ab](https://github.com/Opsolete/Opsolete/commit/05e62ab8f65cd417d427e130682c8d245ba33cab))
* workspace variables beat *.auto.tfvars; admin-only force delete; safer cancel and reporting ([5395132](https://github.com/Opsolete/Opsolete/commit/539513262acd5a29426efe58e33a7ed45eee811b))


### Performance Improvements

* **import:** one masker per finished job; a round's module keeps its name ([53e83e0](https://github.com/Opsolete/Opsolete/commit/53e83e0b215fa76ba0aee9c2ec0a1aade4bbce2f))

## [0.8.0](https://github.com/Opsolete/Opsolete/compare/v0.7.0...v0.8.0) (2026-11-13)


### ⚠ BREAKING CHANGES

* **runner:** RUNNER_TOKEN is no longer read (the server warns when it is set). Register each runner's token before upgrading runners: e.g. `echo "$RUNNER_TOKEN" | opsolete runner-token -from-stdin <name>` keeps a runner's current OPS_RUNNER_TOKEN working. NixOS does this for runnerTokenFile; the Helm chart needs the same step.
* **import:** the Import page and POST /workspaces/{name}/scans are gone; templates import through their module's `existing` input instead.

### Features

* **environments:** keep own, a project value workspaces with their own value don't take ([82d4be8](https://github.com/Opsolete/Opsolete/commit/82d4be84f8b10fd46138b02bef891738a0f16f02))
* **environments:** keep own, and import fixes from a live check ([c2a94e2](https://github.com/Opsolete/Opsolete/commit/c2a94e21dca29f58365ff2d9f02609d039128a26))
* **forms:** one input pipeline, templates at their pinned version, keep-own recorded ([ec90490](https://github.com/Opsolete/Opsolete/commit/ec9049037b421aabe5e8b616db06c70ad5b3de0d))
* **forms:** one input pipeline, templates at their pinned version, keep-own recorded ([7ae4339](https://github.com/Opsolete/Opsolete/commit/7ae43395ea19e29eee99b07d63e5948bc27a8ad0))
* **import:** an import is a session of plan rounds, then one adopted apply ([bab1129](https://github.com/Opsolete/Opsolete/commit/bab112952b68e18e9843aa069b8e8f10b657db56))
* **import:** an import is a session of plan rounds, then one adopted apply ([abd9aac](https://github.com/Opsolete/Opsolete/commit/abd9aac3f112ae7b9d908d6a1d92ded96d97ca4d))
* **import:** Change settings beside Confirm on an import plan ([8fc399d](https://github.com/Opsolete/Opsolete/commit/8fc399d56422362608e808d120afeb9146895fdd))
* **import:** Create new / Import existing on the add and deploy forms ([3f39926](https://github.com/Opsolete/Opsolete/commit/3f39926d62d383ab88533b98e820890af43d36ca))
* **import:** modules import what exists, the plan fills the form ([4924bb2](https://github.com/Opsolete/Opsolete/commit/4924bb22d9a40146fc1a4690993b16282616b76b))
* plan diff on the run page, JSON API /api/v1, secrets kept when a form is refused ([dcd31b4](https://github.com/Opsolete/Opsolete/commit/dcd31b4a5496a09ccd2928b3d57a19f6a208cdf3))
* plan diff on the run page, JSON API /api/v1, secrets kept when a form is refused ([0a0135b](https://github.com/Opsolete/Opsolete/commit/0a0135bb602dd0be5308338e02272d3657a8d0dd))
* **registry:** a shorter module list with search; fix(builder): list fields as a table ([b3312ed](https://github.com/Opsolete/Opsolete/commit/b3312edb6b6a113e8001bcbec29f5e4be6df7f27))
* **registry:** a shorter module list, the rest on each module's page ([837ed6b](https://github.com/Opsolete/Opsolete/commit/837ed6b0a133da851bbd6d997ddc6349bd8f1836))
* **registry:** search the module list, and Sync now on it again ([d883544](https://github.com/Opsolete/Opsolete/commit/d8835443cd04183256ce46344be6f7449adba7cd))
* **runner:** each runner has its own token, and projects pick a runner pool ([497b7c6](https://github.com/Opsolete/Opsolete/commit/497b7c6af5598161d3a7bc23ff7ec8a82447672c))
* **runner:** one-shot runners and a queue endpoint for a pod per run phase ([51b2491](https://github.com/Opsolete/Opsolete/commit/51b24911aa1aff0e7e3adc061af055510698a1c2))
* **runner:** one-shot runners and a queue endpoint for a pod per run phase ([b0055e7](https://github.com/Opsolete/Opsolete/commit/b0055e7df42299d9feef3a473871c42f318fc53d))
* **runner:** opsolete runner-token -from-env registers a token from an environment variable ([24386fb](https://github.com/Opsolete/Opsolete/commit/24386fb61fe03ef5c2d37bd00fab34f82e3bbb59))
* **runner:** opsolete runner-token -from-env registers a token from an environment variable ([b83c3dd](https://github.com/Opsolete/Opsolete/commit/b83c3dd12e5f6c8e98683dee382c139888546fb0))
* **runner:** run each Terraform command in a per-job sandbox ([b449304](https://github.com/Opsolete/Opsolete/commit/b449304c2bb474400a30bd67ae1443507d49f81e))
* **runner:** run each Terraform command in a per-job sandbox ([e117d45](https://github.com/Opsolete/Opsolete/commit/e117d455684dce817d4318072f0d27821bd43035))
* **runs:** Change settings after a run that changed a template and ended without applying ([375ecc9](https://github.com/Opsolete/Opsolete/commit/375ecc9af8346cbf1c5ac5871875fdd4faf2fcf1))
* **runs:** one confirmation rule, typed state machine, promotions as records ([dc50354](https://github.com/Opsolete/Opsolete/commit/dc5035431581dfd1ea1383dd67a563fe13c6158e))
* **runs:** one confirmation rule, typed state machine, promotions as records ([b04b3ac](https://github.com/Opsolete/Opsolete/commit/b04b3acb8aafeaa354454431e76c96d6bcffb62e))
* **ui:** a clearer promote flow ([bf47e8a](https://github.com/Opsolete/Opsolete/commit/bf47e8af61ef25a679fa9d200f704471ad9f3658))
* **ui:** a clearer promote flow ([b53c863](https://github.com/Opsolete/Opsolete/commit/b53c8639f7365ac6ca1f27aad3bea47dda0b765a))
* **workspaces:** configuration versions replace the run's undo ([734a0db](https://github.com/Opsolete/Opsolete/commit/734a0db35389904bd7b8fe26844fdab9e1e23c5f))
* **workspaces:** configuration versions replace the run's undo ([aa4679c](https://github.com/Opsolete/Opsolete/commit/aa4679cbf9d503e20e7014d1ca81400a77112466))


### Bug Fixes

* **builder:** a list's fields as a table across the field card ([b7df86c](https://github.com/Opsolete/Opsolete/commit/b7df86c85af94c7237c1c9c1c8e99b3eae15df8e))
* **forms:** a value that isn't an option is shown, not swapped for the first ([ecbdbd3](https://github.com/Opsolete/Opsolete/commit/ecbdbd3de70ee0bdd0d72d930be634e19793dd58))
* **forms:** warn when importing would change what was found to a project's value ([e8e5f64](https://github.com/Opsolete/Opsolete/commit/e8e5f64fbf4fefec9d9745b764352e5d132131af))
* **import:** a plan that only imports doesn't offer its found values ([bc24cff](https://github.com/Opsolete/Opsolete/commit/bc24cffaa3d16ae0add9b541dfedb79dab0b4e64))
* **import:** adding a template again keeps what was entered for its import plan ([0a78514](https://github.com/Opsolete/Opsolete/commit/0a785144a2cfb5fc545084334c38d9408b3f4ef5))
* **import:** an applied import switches the template's existing off ([c8233db](https://github.com/Opsolete/Opsolete/commit/c8233db0f15d0c924912909adeed5a30572d453f))
* **import:** an ID segment equal to a form value keeps its case ([ea03316](https://github.com/Opsolete/Opsolete/commit/ea0331685f33aa6403ed46d378f58c1006647b4b))
* **import:** Change settings keeps creating runs creating, and ends at the apply ([c4d508c](https://github.com/Opsolete/Opsolete/commit/c4d508c564aca55e550ee28b540d8999a117da68))
* **import:** Change settings, found rows, applied runs ([5fcacc9](https://github.com/Opsolete/Opsolete/commit/5fcacc919412cc180f4fec30b216a189f4e53a2c))
* **import:** found values go to the row of the field that keys every instance ([419654f](https://github.com/Opsolete/Opsolete/commit/419654fc3d9239e1aab98490e1464c447c42f7a6))
* **runner:** keep the state an apply couldn't save sealed with the runner token ([cf99e3c](https://github.com/Opsolete/Opsolete/commit/cf99e3c2afcd6092a46664f4f72de5be7664817b))
* **runner:** keep the state an apply couldn't save sealed with the runner token ([ed71197](https://github.com/Opsolete/Opsolete/commit/ed7119781ff8860ab87df957d6efa6407036c004))
* **runner:** start the sandbox's namespaces from a dumpable helper ([0ca8948](https://github.com/Opsolete/Opsolete/commit/0ca8948b2e9eb92fa1644c4b4ed9268897042f6a))
* **ui:** say why a workspace without configuration has no New run ([f9bdc46](https://github.com/Opsolete/Opsolete/commit/f9bdc46f89eb3df2b16849ba847fff750ea10e59))
* **ui:** say why a workspace without configuration has no New run ([a9154c8](https://github.com/Opsolete/Opsolete/commit/a9154c855059ae2563f4935835191a3bf80bc07e))

## [0.7.0](https://github.com/Opsolete/Opsolete/compare/v0.6.0...v0.7.0) (2026-11-11)


### Features

* adopt existing resources when adding a template ([94a2bb4](https://github.com/Opsolete/Opsolete/commit/94a2bb4c2fb7ae1b2860b03a331673eda0ed0564))
* **builder:** edit adopt entries ([27e66af](https://github.com/Opsolete/Opsolete/commit/27e66afede2dcb03a31ccd8a0ff2874a5622626d))
* **builder:** generate adopt entries from the module's code ([48d2ffd](https://github.com/Opsolete/Opsolete/commit/48d2ffd92a30d746b717ee754f17fa1300f7b019))
* environments and promotion ([2bda81d](https://github.com/Opsolete/Opsolete/commit/2bda81d5c4d18e3ba2061a4f5919e9b3c87e16c1))
* **environments:** per-environment template inputs, set on the project ([7ac40c3](https://github.com/Opsolete/Opsolete/commit/7ac40c38983679101ee461c1f8dd0ea770e26b52))
* import existing cloud resources into workspaces ([8d8bd34](https://github.com/Opsolete/Opsolete/commit/8d8bd340ccf27bc488abf0a5789db832ee8b1641))
* **import:** compact scan results, one line per resource ([09f2f79](https://github.com/Opsolete/Opsolete/commit/09f2f7919ef4d62044a71a81ceed8596420b2b87))
* **import:** follow module calls; find data disks, schedules and NSGs ([5d31041](https://github.com/Opsolete/Opsolete/commit/5d3104108681d84e8717874c0ce2ae54dd0a8bcd))
* **import:** list every resource, in four groups ([7ac9c6b](https://github.com/Opsolete/Opsolete/commit/7ac9c6bb7feee7f5c4bc2626b1ec95b619f02bff))
* **import:** the scan fills in optional parts from what their module imports ([61316cd](https://github.com/Opsolete/Opsolete/commit/61316cd25cedf1104c27f833bc5a0ed6136e7e02))
* **import:** the scan fills in optional parts from what their module imports ([1fb4510](https://github.com/Opsolete/Opsolete/commit/1fb45106802c5411b1cfcb3199ccb28d22a7c1dc))
* **promote:** promote a workspace's templates to another workspace ([917cd7d](https://github.com/Opsolete/Opsolete/commit/917cd7d79e59245f83cf4999202c34152bd7676f))
* **registry:** tag prefix per linked module, registry access for runs ([1ab7035](https://github.com/Opsolete/Opsolete/commit/1ab70358023ea5ed0ae331c8dd303c531c16c426))
* **runner:** fetch registry modules through the runner's server URL ([07483ea](https://github.com/Opsolete/Opsolete/commit/07483ea05b3e1ba8a860c0de263bc54217161e92))
* scan for existing resources and adopt them filled in ([5fee4b1](https://github.com/Opsolete/Opsolete/commit/5fee4b1e68704e57f71cd8781a702742e867b95c))
* **templates:** list inputs, rows of fields from list(object({...})) ([c1cbf28](https://github.com/Opsolete/Opsolete/commit/c1cbf28613c223d1d86f3037d33915400a0a8484))
* **templates:** list, list(string), map(string) and object inputs ([6fbd9a4](https://github.com/Opsolete/Opsolete/commit/6fbd9a401fd0a60f95ce8fed9fb835d97558755b))
* **templates:** list(string), map(string) and object inputs ([0e58430](https://github.com/Opsolete/Opsolete/commit/0e58430bca50cca1e2ffaa89f95d57e4e67c15a8))
* **ui:** search in repository lists ([05cc75d](https://github.com/Opsolete/Opsolete/commit/05cc75dd4f310d816604e3c1b2b20ae0345a37bd))
* **ui:** search in repository lists ([db50897](https://github.com/Opsolete/Opsolete/commit/db50897ec2b8aac2801d733fa472a5e5767609a4))


### Bug Fixes

* 0.x database message names the right versions; drop an unreachable notice ([790fb87](https://github.com/Opsolete/Opsolete/commit/790fb872f6005aad932008b9838a05bf4d9eaa0c))
* **auth:** an admin ending a user's sessions revokes their terraform login tokens too ([5ad24b5](https://github.com/Opsolete/Opsolete/commit/5ad24b5ca0133965face7dbd21a13645086b6cbf))
* **cloud:** locking a remote-execution workspace from the CLI says why ([4fae5b7](https://github.com/Opsolete/Opsolete/commit/4fae5b7166ed8cc6eadfcb722103931ecf0e1590))
* **deps:** bump the go group with 2 updates ([1d7f3e9](https://github.com/Opsolete/Opsolete/commit/1d7f3e9d81cf4c211d4d0fd05250c393d34b9065))
* **deps:** bump the go group with 2 updates ([3775553](https://github.com/Opsolete/Opsolete/commit/377555305b25d91255e1bbb6ca3c60c0378c528e))
* **flake:** update vendorHash ([183e8af](https://github.com/Opsolete/Opsolete/commit/183e8afd262b0248b0190c55bc6c94a39eddefb8))
* **import:** the latest scans list rendered with the run row of another type ([a3663da](https://github.com/Opsolete/Opsolete/commit/a3663da7557f3070b82b2a469b1149f8dd6e2251))
* keep applies' state through server outages; security and upgrade fixes ([30df672](https://github.com/Opsolete/Opsolete/commit/30df6724837b9bba719cbc531e14502a46c1e597))
* keep state an apply couldn't save; graceful shutdown; clear CLI lock error ([79557dc](https://github.com/Opsolete/Opsolete/commit/79557dc51554c8c408568d514042ba9704425665))
* refuse an adoption whose resource a state manages already ([a9618ba](https://github.com/Opsolete/Opsolete/commit/a9618baab8f033d89fd92ffde7919f659ef3c796))
* **runner:** fail jobs of an unknown action instead of planning them ([d80b24e](https://github.com/Opsolete/Opsolete/commit/d80b24ef8751a72c1ac748a52570616ab992e722))
* **runner:** mask the run's state password in its log ([0850b98](https://github.com/Opsolete/Opsolete/commit/0850b9827f3b341f6d4be03161fc69db59802413))
* **runner:** quote a probed row's key in its address ([f515f07](https://github.com/Opsolete/Opsolete/commit/f515f0768da7daf2243c15175e92d98790ae411f))
* **runner:** state an apply couldn't save is pushed again, or kept, not deleted with the workdir ([d66c0bf](https://github.com/Opsolete/Opsolete/commit/d66c0bf0a27c9f41891e49d63044b2d22d443a27))
* **runs:** a new leader waits 1 minute, not 2, before failing expired leases ([34c04d9](https://github.com/Opsolete/Opsolete/commit/34c04d9a2b0817f4bf22733b94d1400ea218adbe))
* **runs:** a server or database outage no longer fails the applies under way ([72089a6](https://github.com/Opsolete/Opsolete/commit/72089a69fa15d3120b4766813667fe57613e3b00))
* **secrets:** key rotation no longer breaks runs under way ([91bb58f](https://github.com/Opsolete/Opsolete/commit/91bb58f68aaefde4376742bcc84063fc7848a100))
* **secrets:** key rotation no longer breaks runs under way ([06d555e](https://github.com/Opsolete/Opsolete/commit/06d555e4d2a14a3b355ea09925b3b4751cc7cc72))
* **server:** SIGTERM ends only log streams and runner claims; other requests drain ([c220b4a](https://github.com/Opsolete/Opsolete/commit/c220b4a621994a98f79aa7141208dc3acf75fb1c))
* **storage:** a newer version's database is no longer called a 0.x one ([2eee3ce](https://github.com/Opsolete/Opsolete/commit/2eee3ce702612bfda21fd9b805beee6aadb0101a))
* **workspaces:** the execution mode can't change while a run is active ([5b5131c](https://github.com/Opsolete/Opsolete/commit/5b5131c40b9dd1a5a9aadf5e2e458b0a57ba5cd7))

## [0.6.0](https://github.com/Opsolete/Opsolete/compare/v0.5.1...v0.6.0) (2026-11-09)


### ⚠ BREAKING CHANGES

* **auth:** existing session cookies are invalid after the upgrade: everyone signs in once.
* **templates:** 0.x template files fail to load: string defaults on number/bool inputs, unknown fields, and "Label: help" is no longer split at runtime.
* **runner:** kill every process a job leaves behind, PID 1 or not
* **runner:** runners and servers must both be 1.0 (older runners never acknowledge).
* **secrets:** values sealed by 0.x don't open; 1.0 starts from a fresh database and state store.
* **access:** users without operate on a project can no longer create workspaces (UI and terraform cloud block); grant their group operate on the default project to keep the old behaviour.
* **storage:** sqlc + native pgx, typed ids, ErrNotFound
* **storage:** Opsolete 1.0 needs a new, empty database.

### Features

* **access:** only admins and project operators create workspaces; settings need operate ([63a0640](https://github.com/Opsolete/Opsolete/commit/63a06402a7bc0d8eb0037aa0c423b46e44ceaae9))
* **account:** add account settings with profile and token revocation ([62a5bfa](https://github.com/Opsolete/Opsolete/commit/62a5bfadf2eb5480513f1ae7a36004b42cef6bc2))
* **auth:** server-side sessions ([8e391a3](https://github.com/Opsolete/Opsolete/commit/8e391a3852f83b2e592ddd72d0ad9c07b9aa982b))
* **builder:** help field and typed default controls; template format 1.0 docs ([515bb0e](https://github.com/Opsolete/Opsolete/commit/515bb0eef71771a7a379bb1dc686c17de649bb25))
* **catalog:** add a commit to a template's open pull request instead of opening another ([181e76a](https://github.com/Opsolete/Opsolete/commit/181e76ae32971815b8d54f5a60e51bcc30b254fe))
* **form:** one striped row per field, label help behind a ?, error states and a busy submit button ([09158b7](https://github.com/Opsolete/Opsolete/commit/09158b77ad5b62d15cd8a272ee6e82a5bc4b5335))
* **http:** static assets and a strict Content-Security-Policy ([6612302](https://github.com/Opsolete/Opsolete/commit/66123029271856b41a717ca71431d22fb0cdc3d8))
* **projects:** add project pages, a project picker and moving workspaces ([bf20182](https://github.com/Opsolete/Opsolete/commit/bf2018290736bca1092eb832cc38850adf789f88))
* **projects:** add the projects data model ([dcbbbd0](https://github.com/Opsolete/Opsolete/commit/dcbbbd08d930e09ad62cba2821a1ee29d831ac9c))
* **projects:** attach variable sets to projects ([6a7106d](https://github.com/Opsolete/Opsolete/commit/6a7106dfd738df37601c653234c2950cbb983f58))
* **projects:** give projects their own menu and a searchable projects page ([69c9ea4](https://github.com/Opsolete/Opsolete/commit/69c9ea42aaf83737f906b3ed54ba6a99d85e72ce))
* **projects:** grant groups access through projects ([9c05d04](https://github.com/Opsolete/Opsolete/commit/9c05d0440fef27c4a2ec135dcbea3b829aaeafdb))
* **projects:** support projects in Terraform's cloud block ([f1561a5](https://github.com/Opsolete/Opsolete/commit/f1561a5671417894902f6939afc23d2e4dfd20ad))
* **registry:** delete modules from the UI ([a546722](https://github.com/Opsolete/Opsolete/commit/a5467224be7ce2ecd83d1e4e46bd41f84ee49b36))
* **runs:** color Terraform plan/apply lines in the live log ([80dd417](https://github.com/Opsolete/Opsolete/commit/80dd417954880309e32366d872b6d0012e0d2cbd))
* **runs:** compact Terraform warnings unless the workspace sets OPS_DEBUG ([f65baba](https://github.com/Opsolete/Opsolete/commit/f65babae3780e49859a799ca1802fbc47d44ef94))
* **runs:** LISTEN/NOTIFY wakes runner claims and live logs; a cap on log streams ([b0cc666](https://github.com/Opsolete/Opsolete/commit/b0cc666fdc344f1711bb5abb83effa9f4746156d))
* **runs:** retention for run logs, drift checks and speculative plans ([70cf4a9](https://github.com/Opsolete/Opsolete/commit/70cf4a92f29451328f050b7ab2c87853fdb00c6b))
* **runs:** show Terraform's own colors in the run log ([6404ede](https://github.com/Opsolete/Opsolete/commit/6404ede6c5aaffbcdba362655960452df3ed112c))
* **security:** mask known secrets in outputs and state diffs, like the logs ([2c1edd5](https://github.com/Opsolete/Opsolete/commit/2c1edd57d3f36fba05810837267e621cb95091b6))
* **server:** one scheduler for background work, run on one replica ([ac2e072](https://github.com/Opsolete/Opsolete/commit/ac2e0725d608fdf901e711f442ed592b98e30eca))
* **settings:** group the settings menu into organization settings sections ([765fc03](https://github.com/Opsolete/Opsolete/commit/765fc03d789ba35799f2d25ac8ca04846554f42c))
* **templates:** preview forms, yes / no conditions and clearer messages for end users ([de47cc9](https://github.com/Opsolete/Opsolete/commit/de47cc965540417c0ec2af4e2437b99e40a88a45))
* **templates:** roll out a template's module version to its workspaces ([d714e20](https://github.com/Opsolete/Opsolete/commit/d714e2038e336a0a71295ff20ffb6ae493187663))
* **templates:** template format 1.0: typed values, help, strict parsing, JSON Schema ([799b687](https://github.com/Opsolete/Opsolete/commit/799b687880be2f5ca5d759e4a57846a1fad6f7db))
* **ui:** own confirmation dialog instead of the browser's confirm(), require JavaScript ([8dee4b0](https://github.com/Opsolete/Opsolete/commit/8dee4b0f845ccc3c0877faa0b2214847d72e58b5))
* **variable-sets:** create a set from a project's or workspace's page ([3448d29](https://github.com/Opsolete/Opsolete/commit/3448d29a7df598eaa96b2510fe2f39f53d3496f5))
* **vcs:** scope VCS connections to projects ([e7495e9](https://github.com/Opsolete/Opsolete/commit/e7495e906e1edc7121670cde4f1c802bd830bcd9))
* **workspaces:** pass workspace and variable-set Terraform variables to added templates ([ec76565](https://github.com/Opsolete/Opsolete/commit/ec765658c028c9b3eda1a271da0078663bdba0ca))
* **workspaces:** remove the root template too ([8571a4c](https://github.com/Opsolete/Opsolete/commit/8571a4c84ccd34c494fd64e78331eb5a80bd22cf))


### Bug Fixes

* atomic settings and selections, lost token replacements, runner transfer limits ([242cb63](https://github.com/Opsolete/Opsolete/commit/242cb637788ebe847710ed9aa559de85b82145f0))
* cap audited form bodies, uncapped replace check, rename of unknown workspace ([ce63f10](https://github.com/Opsolete/Opsolete/commit/ce63f105fed11a30a83952679dbdd03207f31df0))
* **config:** trim spaces around AUTH_DEV_GROUPS entries ([a7e5b6b](https://github.com/Opsolete/Opsolete/commit/a7e5b6b02e74b66698e7620b956cd5c30973c8d3))
* default project warnings, template inputs and template pull requests ([bbdffa7](https://github.com/Opsolete/Opsolete/commit/bbdffa72b6df91fe6e4e131c401827f26f75a42d))
* **deps:** bump golang.org/x/net to v0.62.0 for CVE-2026-78661 ([ff2a8a5](https://github.com/Opsolete/Opsolete/commit/ff2a8a5aea8500180ac8bb4a346d8c4f5ebec423))
* **http:** reject an unparsable form before the handler acts on an empty one ([0741312](https://github.com/Opsolete/Opsolete/commit/0741312f9fad281f7c0c56789410dceef70196c8))
* nix vendorHash and PR_SET_CHILD_SUBREAPER on linux ([12f6650](https://github.com/Opsolete/Opsolete/commit/12f66506334a00012ed6d03e243bd46f8f6be7c7))
* **nix:** let the runner's stop timeout cover Terraform's wait delay after a job timeout ([35b6c72](https://github.com/Opsolete/Opsolete/commit/35b6c72a674874844e3fb3fe7e657fed7ed9870c))
* review fixes for projects, rollouts and sensitive inputs ([e1c4191](https://github.com/Opsolete/Opsolete/commit/e1c419131a4dcdbd69ddcfa9cebb7e679de1399c))
* **runner:** a phase ends within OPS_JOB_TIMEOUT + OPS_JOB_GRACE ([49bb613](https://github.com/Opsolete/Opsolete/commit/49bb613decc1847f6f2afaf095aafdc5e2a03356))
* **runner:** drop a removed template's outputs in its destroy run ([1ee435a](https://github.com/Opsolete/Opsolete/commit/1ee435a13470ca026ec7bbf2476baf6a9aefff85))
* **runner:** kill every process a job leaves behind, PID 1 or not ([59e98f8](https://github.com/Opsolete/Opsolete/commit/59e98f820cb6e924fc4f58f1b59798213c7cf68f))
* **runner:** two-phase claim, so a drain or lost response never loses a job ([c3965d3](https://github.com/Opsolete/Opsolete/commit/c3965d32145c739d69c5de6bfd7d01581d5ae044))
* **runs:** color only Terraform's action symbols, and only in plan output ([85456da](https://github.com/Opsolete/Opsolete/commit/85456da1dbab0345bb9ea6d9e3855c90bc814163))
* **runs:** send a finished run's last log lines before ending the stream ([989997e](https://github.com/Opsolete/Opsolete/commit/989997ee14b9a70f07699acf4e967e189ef1e4ad))
* **runs:** show the log in the apply step once the plan is confirmed ([cd0e1f3](https://github.com/Opsolete/Opsolete/commit/cd0e1f39adfbfd1f8896e295d050603890c309ee))
* **security:** address CodeQL critical and high alerts ([343af10](https://github.com/Opsolete/Opsolete/commit/343af1049f42dfdfeaeb768ec7ab89f91628ad1e))
* **security:** operator-only state diffs, ADMIN_GROUPS, masked secrets in run logs, rotate-key ([e1a9936](https://github.com/Opsolete/Opsolete/commit/e1a9936a854ac171df21471540ca928d632af45b))
* **state:** a rollback records only while it still holds its lock ([0151c59](https://github.com/Opsolete/Opsolete/commit/0151c5916b0d8cf3c4b05b405443281bf1b4c391))
* **state:** drop a run's intermediate state snapshots when it ends ([73e6199](https://github.com/Opsolete/Opsolete/commit/73e6199eb527fd636120db9989bafb6934d143ac))
* **state:** finish state, artifact and job result writes when the server shuts down ([08746d8](https://github.com/Opsolete/Opsolete/commit/08746d88bb5c62dd7b045b72cfcbdc59ab78bc0b))
* **state:** never release a run's state lock as a cloud lock ([4b067e2](https://github.com/Opsolete/Opsolete/commit/4b067e27724774238c3b6b0a228718c9773ba531))
* **templates:** hide show_if fields whose condition fails instead of showing them disabled ([4a5a0a6](https://github.com/Opsolete/Opsolete/commit/4a5a0a6159f606489f481b339255dea7aab4c31c))
* **templates:** keep a new template's id on resave, check browser-incompatible patterns, show numbers in full ([d896bad](https://github.com/Opsolete/Opsolete/commit/d896bad3b8c3c2457ad53d81d9cdde891be3fee1))
* **vcs:** swap the workspace version control page headings ([f50eebf](https://github.com/Opsolete/Opsolete/commit/f50eebfbde3df29bb64964620377ff33fca0477b))
* **workspaces:** a template or root removal no longer marks the workspace destroyed ([be347a2](https://github.com/Opsolete/Opsolete/commit/be347a242161e29f0932ac4043211b47edbb9d74))
* **workspaces:** keep the error of a rejected sensitive input instead of saving the old value ([b971fe8](https://github.com/Opsolete/Opsolete/commit/b971fe86e6a105dd65efeffbc7fbc20d0314a539))
* **workspaces:** put the previous configuration back when a run ends before its apply ([322ed4b](https://github.com/Opsolete/Opsolete/commit/322ed4b8f61b9ba4dd3a63d16201ed13d32e9f56))
* **workspaces:** refuse to delete a workspace while its state is locked ([ac41eed](https://github.com/Opsolete/Opsolete/commit/ac41eed9fd9c583751fee21842554e8f0c485a6e))


### Performance Improvements

* skip known folder-less tags, evict one ETag entry, poll only the run status ([718d6ce](https://github.com/Opsolete/Opsolete/commit/718d6cea37b2c02032d9c7c98b37506510874a44))
* **storage:** index rollout runs, template uses, drift checks and VCS connection workspaces ([b678356](https://github.com/Opsolete/Opsolete/commit/b67835608748ed05e4dd998141f321db31976a8b))


### Code Refactoring

* **secrets:** internal/secrets with key ids; rotation without downtime ([0afc41f](https://github.com/Opsolete/Opsolete/commit/0afc41f2d9d3b88e21c3e3ad04681e62f4ae2c01))
* **storage:** 1.0 schema, one run state machine, one access rule ([ad797bd](https://github.com/Opsolete/Opsolete/commit/ad797bde2eb4995e2a142f14ac166e7c5ced5392))
* **storage:** sqlc + native pgx, typed ids, ErrNotFound ([eae53d9](https://github.com/Opsolete/Opsolete/commit/eae53d98f64318e0ecacd626d4b7d6e2fa306573))

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
