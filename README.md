Balena mobynit package
=====================

The mobynit utility mounts a container union filesystem and pivot roots
a running system into it. It is used as an init replacement in the initramfs
to boot into a hostapp container.

Mobynit uses the hostapp package, a module that discovers container overlay
filesystems by reading overlay2 metadata directly.

## Build

```bash
# Build the mobynit binary (statically linked)
make mobynit

# Build with Docker (includes tests)
./build-docker.sh
```

## Usage

Mobynit is designed to run as PID 1 in an initramfs. It:

1. Mounts the hostapp container (identified by a `current` symlink)
2. Optionally overlays OS block containers (label: `io.balena.image.class=overlay`)
3. Moves existing mounts into the new root
4. Calls `pivot_root` to switch the system root
5. Execs `/sbin/init`

### Command line options

```
mobynit -sysroot=/path  # Mount sysroot and print path (for updates)
mobynit -dataFstype=ext4  # Data partition filesystem type (default: ext4)
mobynit -claimed-abis=/path  # Print claimed kernel ABI ids and exit
```

### Reporting claimed kernel ABIs

`-claimed-abis=<docker data root>` prints the
`io.balena.image.kernel-abi-id` of every OS block container in the store. It
prints one id per line, then exits.

The output is a set. Its order carries no meaning: the sort only makes the
output stable for comparison and for tests. A caller holds the ABI it is
asking about and tests for membership.

The query drops dead containers and containers marked for removal, as the
mount path does. A container that contributes no modules must not keep a
kernel bootable.

A label that is not a lowercase sha256 hex digest is not a claim. The query
logs it and drops it. Callers match whole lines. A label carrying a newline
would otherwise print an id its extension does not own.

A claim is not a guarantee. Activation proved the label against the kernel
bytes, but the pid 1 mount path applies filters this query does not model,
and any of them can still drop the extension after the kexec.

Exit codes are the contract:

| Exit | Meaning |
|------|---------|
| 0, lines on stdout | deployed extensions claim these ABIs |
| 0, no output | no deployed extension claims an ABI |
| non-zero | this binary cannot answer: the argument is empty, or the binary predates the flag |

Two states reach the empty answer. It is the correct answer in both.

A store that mobynit cannot read claims nothing. The query and the pid 1 mount
path read the same store with the same code. A store that fails to read now
also fails at pid 1. That boot mounts no modules.

A pending purge claims nothing. The data partition is wiped after boot, so
mobynit skips every extension overlay on it. The deployed claims would select
a kernel whose modules that boot never mounts.

### Overlay mount ordering

OS block containers (labelled `io.balena.image.class=overlay`) are layered as
overlayfs lowerdirs alongside the hostapp. Their position relative to the
hostapp determines whether they can replace hostapp files or only add new ones.

In overlayfs, `lowerdir=A:B:C` means A has highest lookup priority: a file
in A shadows the same path in B and C.

There are two types of OS blocks:

**Normal extensions** (no `io.balena.image.override` label) mount to the
*right* of the hostapp. They can add new files but cannot replace files that
already exist in the hostapp.

**Override extensions** (`io.balena.image.override=N`) mount to the *left* of
the hostapp. They can replace hostapp files. `N` is the priority — lower
values get higher overlayfs precedence. Equal priorities are ordered by
container name for deterministic boot behaviour.

#### Example

Given a hostapp and three OS blocks:

| Container  | Label                          | Type     |
|------------|--------------------------------|----------|
| hostapp    | —                              | base     |
| networking | `io.balena.image.override=10`  | override |
| security   | `io.balena.image.override=20`  | override |
| extras     | *(none)*                       | normal   |

The resulting overlayfs mount is:

```
lowerdir=networking:security:hostapp:extras
         ^^^^^^^^^^^^^^^^^^^ ^^^^^^^ ^^^^^^
         overrides (sorted)  base    normals
         higher priority ──────────────────> lower priority
```

- `networking` can replace files in all other layers
- `security` can replace hostapp and extras files, but not networking
- `hostapp` can shadow extras files
- `extras` can only contribute files not present in any layer above it

#### Page size limits

The kernel limits mount options to `PAGE_SIZE - 1` bytes. If extensions would
exceed this limit, they are dropped (with a log warning) in reverse order of
importance: normal paths first, then lowest-priority overrides. The hostapp is
never dropped.

### Kernel cmdline options

- `emergency` - Skip OS blocks overlay mounting
- `mobynit.no_overlays` - Skip OS blocks overlay mounting

## Requirements

- overlay2 storage driver (aufs not supported)
- Go 1.22+
