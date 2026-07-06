# ivshmem - Inter-VM Shared Memory

Experimental feature for high-performance host-guest communication via shared memory.

## What is ivshmem?

ivshmem (Inter-VM Shared Memory) is a QEMU/KVM virtual device that enables direct memory sharing between host and guest through a memory-mapped file in `/dev/shm`. This provides significantly faster communication compared to traditional methods like vsock or virtio-serial.

**Performance**: 93-302x faster than vsock (benchmarked in production)

### References

- [QEMU ivshmem Documentation](https://www.qemu.org/docs/master/system/devices/ivshmem.html)
- [KVM Shared Memory](https://www.linux-kvm.org/page/Projects/ivshmem)

## When to Use

ivshmem is ideal for:

- **Real-time monitoring**: Sub-millisecond latency for metrics collection
- **High-frequency communication**: Thousands of operations per second
- **Bulk data transfer**: Multi-GB/s throughput for large datasets
- **eBPF observability**: Stream tracing data from guest to host
- **Performance-critical paths**: When vsock latency is a bottleneck

**Not suitable for**:
- Traditional request-response patterns (use vsock)
- Infrequent communication (overhead not justified)
- Cases where TCP/IP semantics are required

## Why Use ivshmem?

### Performance Comparison

| Method | Latency | Throughput | Use Case |
|--------|---------|------------|----------|
| vsock | 7.7 μs | 125 MB/s | General IPC, networking |
| ivshmem | 0.08 μs | 25,000 MB/s | Hot paths, bulk transfer |

**When you need**:
- **Low latency**: Sub-microsecond response times
- **High throughput**: GB/s data rates
- **Minimal overhead**: Direct memory access without context switches

## Architecture

```
┌─────────────────────────────────────┐
│          Host Process               │
│                                     │
│  mmap(/dev/shm/ivshmem-{id})       │
│         │                           │
│         ▼                           │
│  ┌─────────────────┐                │
│  │  Shared Memory  │                │
│  │     (1MB)       │                │
│  └─────────────────┘                │
│         │                           │
└─────────┼───────────────────────────┘
          │
          │ PCI Device
          │
┌─────────┼───────────────────────────┐
│         ▼                           │
│  ┌─────────────────┐                │
│  │  Shared Memory  │                │
│  │   (same 1MB)    │                │
│  └─────────────────┘                │
│         │                           │
│         ▼                           │
│   Guest Process                     │
│                                     │
└─────────────────────────────────────┘
```

- **Zero-copy**: Both sides access the same physical memory
- **Direct access**: No kernel involvement for reads/writes
- **Synchronization**: Application manages locks/atomics

## Quick Start

### 1. Enable ivshmem

```python
from cubesandbox import Sandbox

sandbox = Sandbox.create(
    template="your-template-id",
    metadata={
        "enable_ivshmem": "true"  # Opt-in to ivshmem
    }
)
```

### 2. Access from Host

```python
import mmap

sandbox_id = sandbox.id
shm_path = f"/dev/shm/ivshmem-{sandbox_id}"

# Memory-map the shared region
with open(shm_path, "r+b") as f:
    mm = mmap.mmap(f.fileno(), 1024 * 1024)  # 1MB
    
    # Write data (host → guest)
    mm[0:13] = b"Hello, Guest!"
    
    # Read data (guest → host)
    response = mm[100:113]
    print(f"Guest says: {response}")
    
    mm.close()
```

### 3. Access from Guest

Inside the guest VM, the shared memory appears as a PCI device:

```bash
# Find the ivshmem device
lspci | grep "Inter-VM shared memory"
# Output: 00:04.0 RAM memory: Red Hat, Inc. Inter-VM shared memory
```

**C code in guest**:

```c
#include <sys/mman.h>
#include <fcntl.h>
#include <string.h>

int main() {
    // Open shared memory
    int fd = open("/dev/shm/ivshmem", O_RDWR);
    
    // Map into process memory
    void *addr = mmap(NULL, 1024 * 1024, PROT_READ | PROT_WRITE,
                      MAP_SHARED, fd, 0);
    
    // Read message from host
    char *shm = (char *)addr;
    printf("Host says: %s\n", shm);
    
    // Write response
    strcpy(shm + 100, "Hello, Host!");
    
    munmap(addr, 1024 * 1024);
    close(fd);
    return 0;
}
```

## Usage Patterns

### Pattern 1: Metrics Collection

**Host monitoring loop**:

```python
import struct
import time

with open(f"/dev/shm/ivshmem-{sandbox_id}", "r+b") as f:
    mm = mmap.mmap(f.fileno(), 1024 * 1024)
    
    while True:
        # Read metrics written by guest
        cpu = struct.unpack('f', mm[0:4])[0]
        mem = struct.unpack('f', mm[4:8])[0]
        
        print(f"CPU: {cpu:.1f}%, Memory: {mem:.1f}%")
        time.sleep(0.1)  # Poll every 100ms
```

**Guest agent** (writes metrics):

```c
float cpu = get_cpu_usage();
float mem = get_memory_usage();

memcpy(shm + 0, &cpu, sizeof(float));
memcpy(shm + 4, &mem, sizeof(float));
```

### Pattern 2: Ring Buffer for Events

```python
# Shared layout (agreed between host and guest)
# Offset 0-3: write_pos (u32)
# Offset 4-7: read_pos (u32)
# Offset 8+: circular buffer (1MB - 8 bytes)

HEADER_SIZE = 8
BUFFER_SIZE = 1024 * 1024 - HEADER_SIZE

def consume_events(mm):
    read_pos = struct.unpack('I', mm[4:8])[0]
    write_pos = struct.unpack('I', mm[0:4])[0]
    
    while read_pos != write_pos:
        # Read event at read_pos
        event_offset = HEADER_SIZE + (read_pos % BUFFER_SIZE)
        event_data = mm[event_offset:event_offset+64]
        
        process_event(event_data)
        
        # Advance read pointer
        read_pos = (read_pos + 64) % BUFFER_SIZE
        struct.pack_into('I', mm, 4, read_pos)
```

## Memory Layout Planning

### Recommended Structure (1MB)

```
Offset    Size    Purpose
------------------------------
0         4KB     Host → Guest control/data
4KB       4KB     Guest → Host status/metrics
8KB       16KB    Ring buffer for events
24KB      1000KB  Bulk data transfer
```

### Protocol Design Tips

1. **Use atomic operations** for shared counters
2. **Add checksums** for data integrity
3. **Version your protocol** for compatibility
4. **Document layout** clearly for both sides

## Benchmarking

Run the included benchmark to measure performance on your system:

```bash
python3 examples/ivshmem/ivshmem_benchmark.py --sandbox-id YOUR_SANDBOX_ID
```

**Expected output**:

```
Latency: 0.078 μs
Throughput (sequential): 25,615 MB/s
IOPS (4KB random): 77,616
```

## Security Considerations

### Trust Boundary

Shared memory crosses the host-guest trust boundary. Always:

1. **Validate guest data**:
   ```python
   value = struct.unpack('i', mm[0:4])[0]
   if not (0 <= value <= MAX_VALUE):
       raise ValueError("Invalid value from guest")
   ```

2. **Use checksums**:
   ```python
   data = mm[0:100]
   checksum = hashlib.sha256(data).digest()
   if checksum != mm[100:132]:
       raise ValueError("Data corruption")
   ```

3. **Rate limit**:
   ```python
   if time.time() - last_update < MIN_INTERVAL:
       return  # Ignore too-frequent updates
   ```

### File Security

- **Permissions**: 0o600 (owner read/write only)
- **Location**: `/dev/shm` (tmpfs, memory-backed)
- **Lifecycle**: Created on sandbox start, deleted on stop

## Limitations

### Current (v0.6.x)

- **Fixed size**: 1MB per sandbox (not configurable)
- **Single region**: One shared memory area only
- **Manual management**: Application handles synchronization
- **Experimental**: API may change based on feedback

### Future Roadmap

- Configurable size via `metadata.ivshmem_size`
- Multiple named regions per sandbox
- Guest-side helper library
- Hot-add/remove support

## Troubleshooting

### File not found

```bash
# Verify ivshmem is enabled
ls -la /dev/shm/ivshmem-*

# Check sandbox metadata
# Should contain: metadata={"enable_ivshmem": "true"}
```

### Permission denied

```bash
# Shared memory is owned by root (shim process)
sudo ls -la /dev/shm/ivshmem-{sandbox_id}
```

### Guest can't see device

```bash
# In guest, check PCI devices
lspci | grep -i "shared memory"

# Verify ivshmem driver is available
modprobe ivshmem
```

### Poor performance

1. Ensure `/dev/shm` is on tmpfs (in RAM)
2. Avoid busy-waiting (use appropriate delays)
3. Run benchmark to establish baseline

## Migration from vsock

If you're using vsock and considering ivshmem:

### When to migrate

- **High-frequency polling**: vsock adds 7.7 μs per call
- **Bulk transfer**: vsock limited to ~125 MB/s
- **Real-time requirements**: Need sub-millisecond latency

### Hybrid approach (recommended)

Use both:
- **vsock**: Connection setup, commands, control messages
- **ivshmem**: Hot path data, metrics, bulk transfer

```python
# Control via vsock
vsock_send({"command": "start_monitoring"})

# Data via ivshmem
while monitoring:
    metrics = read_from_ivshmem()
    process(metrics)
```

## Files

- `ivshmem_benchmark.py` - Performance benchmark tool
- `README.md` - This file

## References

- [QEMU ivshmem device](https://www.qemu.org/docs/master/system/devices/ivshmem.html)
- [Cube-Sandbox documentation](../../docs/)

## Status

**Experimental** - This feature is under active development. API and behavior may change based on user feedback.

Please report issues or feedback via GitHub issues.
