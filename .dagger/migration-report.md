# Migration Report

## .dagger/modules/e2e requires explicit loading

The module at `.dagger/modules/e2e` is still valid, but it must be loaded explicitly.

- **This works**: `dagger -m .dagger/modules/e2e call --help`
- **This no longer works**: `cd .dagger/modules/e2e; dagger call --help`

ACTION: If your scripts rely on implicit loading of `.dagger/modules/e2e`, change them to use explicit loading.

## Root module requires explicit loading

The root module is still valid, but it must be loaded explicitly.

- **This works**: `dagger -m . call --help`
- **This no longer works**: `dagger call --help`

ACTION: If your scripts rely on implicit loading of the root module, change them to use explicit loading.
