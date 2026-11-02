/*
 * hostmask.c -- host-identity masking shim for sandboxd.
 *
 * WHAT THIS IS
 *   A small dynamically-linked shared object (a "preload shim") that intercepts
 *   the syscall-reachable facts by which a jailed workload could identify the
 *   host it runs on, and answers with the WORKSPACE's limits instead:
 *
 *     uname(2)       release/version/nodename  -> a generic sandbox kernel
 *     sysconf(3)     _SC_NPROCESSORS_*          -> the workspace's cpu_cores
 *                    _SC_PHYS_PAGES/_SC_AVPHYS_PAGES -> the workspace's memory
 *     sysinfo(2)     totalram/freeram/procs     -> the workspace's memory/cpus
 *     sched_getaffinity(2) is reduced to the workspace's cpu_cores, so a
 *                    consumer that counts CPUs (nproc, node's os.cpus()) sees
 *                    the workspace's count even when it does not call sysconf.
 *     getauxval(3)   AT_HWCAP/AT_HWCAP2         -> 0 (no host CPU fingerprint)
 *
 * WHY IT IS REACHED
 *   It is named in /etc/ld.so.preload, which the dynamic loader reads for EVERY
 *   dynamically-linked exec, independent of the LD_PRELOAD environment variable.
 *   A tenant therefore cannot disable it with `env -u LD_PRELOAD` or `env -i`
 *   (measured). The file and the shim are bind-mounted READ-ONLY by nsjail, so a
 *   tenant cannot replace them.
 *
 * HONEST LIMITS (documented in docs/DECISIONS.md, do NOT remove)
 *   - A STATICALLY-LINKED binary does not run the dynamic loader at all, so it
 *     is not intercepted and sees the real host values.
 *   - A non-glibc C library (musl, e.g. an Alpine-built binary) is loaded by
 *     musl's own loader, which does not read /etc/ld.so.preload, so it is not
 *     intercepted either.
 *   - A direct syscall (syscall(2) with a raw uname/sysinfo) bypasses libc.
 *   These are properties of preload-based interposition, not bugs; the masking
 *   layer is a defense-in-depth measure, not a security boundary. The /proc and
 *   /sys filesystem views remain hidden by the mount namespace, and those are
 *   what the acceptance suite asserts are unreadable.
 *
 * CONFIGURATION
 *   The shim reads its answers from the environment:
 *     SANDBOXD_MASK_CPUS            decimal cpu count
 *     SANDBOXD_MASK_MEM_BYTES       decimal bytes of memory
 *     SANDBOXD_MASK_KERNEL_RELEASE  uname.release value
 *     SANDBOXD_MASK_KERNEL_VERSION  uname.version value
 *     SANDBOXD_MASK_NODENAME        uname.nodename value
 *   If a variable is missing the shim falls back to a conservative constant
 *   (1 cpu, 256 MB), so unsetting the environment cannot restore the host's
 *   values -- only a raw-syscall/static/musl bypass can.
 *
 * REBUILD
 *   `make shim` rebuilds internal/executor/hostmask.so from this source. The
 *   Dockerfile rebuilds it from source at image build; the committed .so is the
 *   same source prebuilt so a plain `go build ./...` can embed it without a
 *   compiler.
 */
#define _GNU_SOURCE
#include <dlfcn.h>
#include <sched.h>
#include <stdlib.h>
#include <string.h>
#include <sys/auxv.h>
#include <sys/sysinfo.h>
#include <sys/utsname.h>
#include <unistd.h>

#define SANDBOXD_FALLBACK_CPUS 1L
#define SANDBOXD_FALLBACK_MEM (256L * 1024 * 1024)

/* env_long reads a decimal integer from the environment, or returns d. */
static long env_long(const char *key, long d) {
  const char *v = getenv(key);
  if (!v || !*v)
    return d;
  char *end = 0;
  long x = strtol(v, &end, 10);
  return (end && *end == '\0') ? x : d;
}

/* env_str reads a string from the environment, or returns d. */
static const char *env_str(const char *key, const char *d) {
  const char *v = getenv(key);
  return (v && *v) ? v : d;
}

/* copy_field writes src into a fixed-size uname field, always NUL-terminated. */
static void copy_field(char *dst, size_t n, const char *src) {
  strncpy(dst, src, n - 1);
  dst[n - 1] = '\0';
}

typedef int (*uname_fn)(struct utsname *);

int uname(struct utsname *buf) {
  static uname_fn real;
  if (!real)
    real = (uname_fn)dlsym(RTLD_NEXT, "uname");
  int r = real(buf);
  if (r == 0) {
    copy_field(buf->release, sizeof(buf->release),
               env_str("SANDBOXD_MASK_KERNEL_RELEASE", "0.0.0-sandbox"));
    copy_field(buf->version, sizeof(buf->version),
               env_str("SANDBOXD_MASK_KERNEL_VERSION", "#1 SMP"));
    copy_field(buf->nodename, sizeof(buf->nodename),
               env_str("SANDBOXD_MASK_NODENAME", "sandbox"));
  }
  return r;
}

typedef long (*sysconf_fn)(int);

long sysconf(int name) {
  static sysconf_fn real;
  if (!real)
    real = (sysconf_fn)dlsym(RTLD_NEXT, "sysconf");
  switch (name) {
  case _SC_NPROCESSORS_ONLN:
  case _SC_NPROCESSORS_CONF:
    return env_long("SANDBOXD_MASK_CPUS", SANDBOXD_FALLBACK_CPUS);
  case _SC_PHYS_PAGES:
  case _SC_AVPHYS_PAGES: {
    long page = real(_SC_PAGE_SIZE);
    if (page <= 0)
      page = 4096;
    return env_long("SANDBOXD_MASK_MEM_BYTES", SANDBOXD_FALLBACK_MEM) / page;
  }
  default:
    return real(name);
  }
}

typedef int (*sysinfo_fn)(struct sysinfo *);

int sysinfo(struct sysinfo *si) {
  static sysinfo_fn real;
  if (!real)
    real = (sysinfo_fn)dlsym(RTLD_NEXT, "sysinfo");
  int r = real(si);
  if (r == 0) {
    long mem = env_long("SANDBOXD_MASK_MEM_BYTES", SANDBOXD_FALLBACK_MEM);
    long cpus = env_long("SANDBOXD_MASK_CPUS", SANDBOXD_FALLBACK_CPUS);
    /* Hide host uptime and load average too: both are host-wide signals. */
    si->uptime = 0;
    si->loads[0] = si->loads[1] = si->loads[2] = 0;
    si->totalram = (unsigned long)mem;
    si->freeram = (unsigned long)(mem / 2);
    si->sharedram = 0;
    si->bufferram = 0;
    si->totalswap = 0;
    si->freeswap = 0;
    si->procs = (unsigned short)cpus;
  }
  return r;
}

typedef int (*affinity_fn)(pid_t, size_t, cpu_set_t *);

/*
 * sched_getaffinity is reduced to at most SANDBOXD_MASK_CPUS cpus. This is a
 * real reduction of the returned mask (not a fabricated number), so a consumer
 * that counts set bits sees the workspace's cpu count. It cannot add cpus the
 * host does not have; it only drops the extras the host exposes.
 */
int sched_getaffinity(pid_t pid, size_t size, cpu_set_t *set) {
  static affinity_fn real;
  if (!real)
    real = (affinity_fn)dlsym(RTLD_NEXT, "sched_getaffinity");
  int r = real(pid, size, set);
  if (r != 0)
    return r;
  long want = env_long("SANDBOXD_MASK_CPUS", SANDBOXD_FALLBACK_CPUS);
  if (want <= 0)
    want = 1;
  long kept = 0;
  for (long i = 0; i < CPU_SETSIZE; i++) {
    if (CPU_ISSET(i, set)) {
      kept++;
      if (kept > want)
        CPU_CLR(i, set);
    }
  }
  return 0;
}

typedef unsigned long (*getauxval_fn)(unsigned long);

unsigned long getauxval(unsigned long type) {
  static getauxval_fn real;
  if (!real)
    real = (getauxval_fn)dlsym(RTLD_NEXT, "getauxval");
  /* CPU capability bits are a host fingerprint; report none of them. */
  if (type == AT_HWCAP || type == AT_HWCAP2)
    return 0;
  return real(type);
}

/*
 * libc version strings carry the distro patch level ("2.39-0ubuntu8.9"), which
 * identifies the base image. Reduce them to the upstream major.minor, which is
 * still true but no longer a distro fingerprint. A caller that only wants the
 * upstream number (the common case) is unaffected.
 */
static void strip_distro_suffix(char *dst, const char *src, size_t n) {
  size_t i = 0;
  /* Copy up to the first hyphen, i.e. "2.39-0ubuntu8.9" -> "2.39". A space is
   * left intact, because "glibc 2.39" (confstr) separates the library name from
   * its version with one. */
  while (i < n - 1 && src[i] && src[i] != '-') {
    dst[i] = src[i];
    i++;
  }
  dst[i] = '\0';
}

typedef size_t (*confstr_fn)(int, char *, size_t);

size_t confstr(int name, char *buf, size_t len) {
  static confstr_fn real;
  if (!real)
    real = (confstr_fn)dlsym(RTLD_NEXT, "confstr");
  if (name == _CS_GNU_LIBC_VERSION) {
    char tmp[64];
    /* Ask the real confstr for the full string, then trim the suffix. A
     * probing call (buf == NULL or len == 0) returns the trimmed length. */
    size_t got = real(name, tmp, sizeof(tmp));
    if (got == 0)
      return got;
    char trimmed[64];
    strip_distro_suffix(trimmed, tmp, sizeof(trimmed));
    size_t need = strlen(trimmed) + 1;
    if (!buf || len == 0)
      return need;
    if (len >= need) {
      memcpy(buf, trimmed, need);
    } else {
      memcpy(buf, trimmed, len - 1);
      buf[len - 1] = '\0';
    }
    return need;
  }
  return real(name, buf, len);
}

const char *gnu_get_libc_version(void) {
  static const char *(*real)(void);
  if (!real)
    real = (const char *(*)(void))dlsym(RTLD_NEXT, "gnu_get_libc_version");
  static char trimmed[64];
  strip_distro_suffix(trimmed, real(), sizeof(trimmed));
  return trimmed;
}
