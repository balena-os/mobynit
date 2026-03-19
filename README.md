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
```

### Kernel cmdline options

- `emergency` - Skip OS blocks overlay mounting
- `mobynit.no_overlays` - Skip OS blocks overlay mounting

## Requirements

- overlay2 storage driver (aufs not supported)
- Go 1.22+
