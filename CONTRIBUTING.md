# NVIDIA Kubernetes Launch Kit

## How to Contribute

NVIDIA Kubernetes Launch Kit is [Apache 2.0 licensed](LICENSE) and accepts contributions via GitHub pull requests.
This document outlines some of the conventions on development workflow, commit message formatting,
contact points and other resources to make it easier to get your contribution accepted.

## Development Guide

Read [AGENTS.md](AGENTS.md) for repository navigation, behavior contracts,
build and test guidance, and the development workflow. These shared rules also
apply to agent-assisted contributions; `CLAUDE.md` points to the same guide.

### Continuous integration

Build, test, and lint jobs use `Mellanox/cloud-orchestration-reusable-workflows`.
Go and golangci-lint versions are selected centrally. CI retains documentation
contract checks and cross-platform builds, and combines race detection and
coverage in one test run. `make lint` installs a versioned local binary;
`GOLANGCILINT_VERSION` can select the same version used by CI.

### Extension Contracts

Use the [contract index](openspec/README.md) to identify the integration boundaries
of a change. Explain relevant extension decisions and verification in the PR;
update specs when shared behavior or a documented exception changes. Ordinary
conforming changes do not require a new spec or an OpenSpec proposal. See the
index for optional pinned structural validation and its limits.

### Documentation in Every Change

Update the relevant documentation sections in the same PR when changing
behavior, commands, configuration, interfaces or development procedures. Use
the [documentation impact map](AGENTS.md#required-documentation-updates) to
identify affected guides, README examples, architecture diagrams and skill
playbooks, including their bundled references.

Before opening a PR:

- Update existing sections and examples to describe the resulting behavior.
- Check examples, links and any affected generated documentation.
- Run `make build` and `python3 scripts/check-docs.py --binary build/l8k` to
  check documented flags, navigation and offline generation examples.
- For site changes, install `requirements-docs.txt` in an isolated Python
  environment and run `mkdocs build --strict`.
- List documentation updates and validation in the PR description. If no
  documentation update is needed, explain why existing guidance remains accurate.

## Coding Style

Please follows the standard formatting recommendations and language idioms set out in [Effective Go](https://golang.org/doc/effective_go.html) and in the [Go Code Review Comments wiki](https://github.com/golang/go/wiki/CodeReviewComments).

## Format of the patch

Each patch is expected to comply with the following format:

```text
Change summary

More detailed explanation of your changes: Why and how.
Wrap it to 72 characters.
See [here] (http://chris.beams.io/posts/git-commit/)
for some more good advices.

[Fixes #NUMBER (or URL to the issue)]
```

For example:

```text
Fix poorly named identifiers
  
One identifier, fnname, in func.go was poorly named.  It has been renamed
to fnName.  Another identifier retval was not needed and has been removed
entirely.

Fixes #1
```

## Certificate of Origin

In order to get a clear contribution chain of trust we use the [signed-off-by language](https://01.org/community/signed-process)
used by the Linux kernel project.

DCO can be found [here](https://developercertificate.org/)

## Contributing Code

* Make sure to create an [Issue](https://github.com/NVIDIA/k8s-launch-kit/issues) for bug fix or the feature request.
* **For bugs**: For the bug fixes, please follow the issue template format while creating a issue.  If you have already found a fix, feel free to submit a Pull Request referencing the Issue you created. Include the `Fixes #` syntax to link it to the issue you're addressing.
* **For feature requests**, For the feature requests, please follow the issue template format while creating a feature requests.
  * Please make sure each PR are compiling and CI checks are passing.
  * In order to ensure your PR can be reviewed in a timely manner, please keep PRs small.

Once you're ready to contribute code back to this repo, start with these steps:

* Fork the project.
* Clone the fork to your machine:

```shell
git clone https://github.com/NVIDIA/k8s-launch-kit.git
```

* Create a topic branch with prefix `dev/` for your change and checkout that branch:

```shell
git checkout -b dev/some-topic-branch
```

* Make your changes to the code and add tests to cover contributed code.
* Run `make build && make test` to validate it builds and will not break current functionality.
* Commit your changes and push them to your fork.
* Open a pull request for the appropriate project.
* Maintainers will review your pull request, suggest changes, run tests and eventually merge or close the request.
