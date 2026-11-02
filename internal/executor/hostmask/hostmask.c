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
 *     dlsym(3) of a masked symbol (any handle)  -> the shim's own function, so
 *                    a handle-scoped lookup cannot step behind the shim.
 *
 * WHY IT IS REACHED
 *   It is named in /etc/ld.so.preload, which the dynamic loader reads for EVERY
 *   dynamically-linked exec, independent of the LD_PRELOAD environment variable.
 *   A tenant therefore cannot disable it with `env -u LD_PRELOAD` or `env -i`
 *   (measured). The file and the shim are bind-mounted READ-ONLY by nsjail, so a
 *   tenant cannot replace them.
 *
 * THE dlopen-SCOPED LOOKUP (closed; this is NOT an inherent limit)
 *   `dlopen("libc.so.6")` returns a SPECIFIC object handle, and `dlsym` on that
 *   handle normally searches only that object's local scope -- which skips this
 *   shim. The shim therefore interposes `dlsym` itself: a request to resolve one
 *   of the masked symbols from ANY handle is answered with the shim's own
 *   function, the same value the RTLD_DEFAULT path already returns. This is
 *   idiomatic dlsym interposition (the symbol interposed is the same symbol that
 *   is being looked up), measured to close CDLL("libc.so.6"),
 *   CDLL("/lib/.../libc.so.6"), libc.dlopen+dlsym, dlmopen, and
 *   dlsym(RTLD_NEXT, "uname"). The residual is a handle-scoped dlvsym(3) call;
 *   see the block below on the bootstrap for why dlvsym is not interposed here.
 *   See docs/VERIFICATION.md for the measured list.
 *
 * HONEST LIMITS (documented in docs/DECISIONS.md, do NOT remove)
 *   - A STATICALLY-LINKED binary does not run the dynamic loader at all, so it
 *     is not intercepted and sees the real host values.
 *   - A non-glibc C library (musl, e.g. an Alpine-built binary) is loaded by
 *     musl's own loader, which does not read /etc/ld.so.preload, so it is not
 *     intercepted either.
 *   - A direct syscall (syscall(2) with a raw uname/sysinfo) bypasses libc.
 *   The first two are properties of preload-based interposition; the raw syscall
 *   is a property of the kernel ABI. The masking layer is defense-in-depth, not
 *   a security boundary. The /proc and /sys filesystem views remain hidden by
 *   the mount namespace, and those are what the acceptance suite asserts are
 *   unreadable.
 *
 * CONFIGURATION
 *   The shim reads its answers from the environment. The variable names are
 *   deliberately opaque and engine-neutral: publishing an engine-named variable
 *   to the workspace would announce both the engine and the fact that a mask is
 *   active (a tenant can read its own environment). The names are:
 *     LSM_SYS_CPUS   decimal cpu count
 *     LSM_SYS_MEM    decimal bytes of memory
 *     LSM_SYS_REL    uname.release value
 *     LSM_SYS_VER    uname.version value
 *     LSM_SYS_NODE   uname.nodename value
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

/* Forward declarations of every function this shim interposes. They are needed
 * before the dlsym interposer, which hands their addresses back to a caller. */
int uname(struct utsname *buf);
long sysconf(int name);
int sysinfo(struct sysinfo *si);
int sched_getaffinity(pid_t pid, size_t size, cpu_set_t *set);
unsigned long getauxval(unsigned long type);
size_t confstr(int name, char *buf, size_t len);
const char *gnu_get_libc_version(void);

/*
 * THE dlopen-SCOPED dlsym BYPASS, AND WHY IT IS CLOSED BY dlsym INTERPOSITION.
 *
 * `dlopen("libc.so.6")` returns a SPECIFIC object handle. `dlsym(handle, ...)`
 * on it searches libc's own local scope and deliberately SKIPS this preload
 * shim, so `ctypes.CDLL("libc.so.6", use_errno=True).uname(buf)` returned the
 * REAL host kernel with no raw syscall at all. That is a gap in the shim, not
 * an inherent limit of preload interposition.
 *
 * The shim therefore interposes `dlsym` itself: a request for one of the
 * masked symbols is answered with the shim's own function, exactly the value
 * the RTLD_DEFAULT path already returns, for ANY handle. Every other lookup
 * passes straight through, so no program is disturbed. Measured to close
 * CDLL("libc.so.6"), CDLL("/lib/.../libc.so.6"), libc.dlopen+dlsym,
 * dlsym(RTLD_NEXT,...) and the default handle (docs/VERIFICATION.md).
 *
 * BOOTSTRAP. The shim's OWN lookups must reach real libc without re-entering
 * this interposer. They use `dlvsym(RTLD_NEXT, ...)`, and this shim deliberately
 * does NOT interpose `dlvsym` -- interposing both `dlsym` and `dlvsym` on this
 * glibc makes the loader's own bootstrap mutually recursive (a versioned
 * reference can bind back to this object's unversioned definition), which
 * crashes at process start. The RESIDUAL is therefore a handle-scoped
 * `dlvsym(handle, "uname", "GLIBC_2.2.5")` lookup, which requires compiled code
 * (ctypes does not expose dlvsym) and is documented honestly in ADR 72.
 */
typedef void *(*dlsym_fn)(void *, const char *);
static dlsym_fn real_dlsym;

static void *shim_next(const char *sym) {
  if (!real_dlsym)
    real_dlsym = (dlsym_fn)dlvsym(RTLD_NEXT, "dlsym", "GLIBC_2.2.5");
  return real_dlsym(RTLD_NEXT, sym);
}

/*
 * is_masked_symbol reports whether a looked-up symbol is one whose answer the
 * shim controls. It includes `dlsym` itself so a handle-scoped lookup of the
 * loader API also resolves to the shim rather than stepping behind it.
 */
static int is_masked_symbol(const char *s) {
  return s && (!strcmp(s, "uname") || !strcmp(s, "sysconf") ||
               !strcmp(s, "sysinfo") || !strcmp(s, "sched_getaffinity") ||
               !strcmp(s, "getauxval") || !strcmp(s, "confstr") ||
               !strcmp(s, "gnu_get_libc_version") || !strcmp(s, "dlsym"));
}

/*
 * dlsym interposes the symbol it is itself asked to resolve. A handle-scoped
 * lookup of a masked symbol -- e.g. ctypes.CDLL("libc.so.6").uname, which
 * resolves `uname` from the libc handle -- is answered with the shim's own
 * function, exactly what the RTLD_DEFAULT path already returns. Every other
 * lookup passes straight through to the real dlsym, so this cannot disturb a
 * program. This closes a bypass that is NOT a raw syscall and therefore NOT an
 * inherent limit of preload interposition (see docs/DECISIONS.md ADR 72).
 */
void *dlsym(void *handle, const char *symbol) {
  if (is_masked_symbol(symbol)) {
    if (!strcmp(symbol, "uname"))
      return (void *)&uname;
    if (!strcmp(symbol, "sysconf"))
      return (void *)&sysconf;
    if (!strcmp(symbol, "sysinfo"))
      return (void *)&sysinfo;
    if (!strcmp(symbol, "sched_getaffinity"))
      return (void *)&sched_getaffinity;
    if (!strcmp(symbol, "getauxval"))
      return (void *)&getauxval;
    if (!strcmp(symbol, "confstr"))
      return (void *)&confstr;
    if (!strcmp(symbol, "gnu_get_libc_version"))
      return (void *)&gnu_get_libc_version;
    return (void *)&dlsym;
  }
  if (!real_dlsym)
    real_dlsym = (dlsym_fn)dlvsym(RTLD_NEXT, "dlsym", "GLIBC_2.2.5");
  return real_dlsym(handle, symbol);
}

#define ENGINE_FALLBACK_CPUS 1L
#define ENGINE_FALLBACK_MEM (256L * 1024 * 1024)

/* Opaque fallback kernel strings. They are deliberately non-identifying: a
 * generic uname.release/version that names neither the engine nor a distro, so
 * `strings` over the shim yields no fingerprint. They match the defaults in
 * internal/config (DefaultMaskKernelRelease/Version) so `env -i` -- which
 * clears the LSM_SYS_* variables -- still answers with the same generic values. */
#define ENGINE_FALLBACK_REL "0.0.0-sandbox"
#define ENGINE_FALLBACK_VER "#1 SMP"
#define ENGINE_FALLBACK_NODE "sandbox"

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
    real = (uname_fn)shim_next("uname");
  int r = real(buf);
  if (r == 0) {
    copy_field(buf->release, sizeof(buf->release),
               env_str("LSM_SYS_REL", ENGINE_FALLBACK_REL));
    copy_field(buf->version, sizeof(buf->version),
               env_str("LSM_SYS_VER", ENGINE_FALLBACK_VER));
    copy_field(buf->nodename, sizeof(buf->nodename),
               env_str("LSM_SYS_NODE", ENGINE_FALLBACK_NODE));
  }
  return r;
}

typedef long (*sysconf_fn)(int);

long sysconf(int name) {
  static sysconf_fn real;
  if (!real)
    real = (sysconf_fn)shim_next("sysconf");
  switch (name) {
  case _SC_NPROCESSORS_ONLN:
  case _SC_NPROCESSORS_CONF:
    return env_long("LSM_SYS_CPUS", ENGINE_FALLBACK_CPUS);
  case _SC_PHYS_PAGES:
  case _SC_AVPHYS_PAGES: {
    long page = real(_SC_PAGE_SIZE);
    if (page <= 0)
      page = 4096;
    return env_long("LSM_SYS_MEM", ENGINE_FALLBACK_MEM) / page;
  }
  default:
    return real(name);
  }
}

typedef int (*sysinfo_fn)(struct sysinfo *);

int sysinfo(struct sysinfo *si) {
  static sysinfo_fn real;
  if (!real)
    real = (sysinfo_fn)shim_next("sysinfo");
  int r = real(si);
  if (r == 0) {
    long mem = env_long("LSM_SYS_MEM", ENGINE_FALLBACK_MEM);
    long cpus = env_long("LSM_SYS_CPUS", ENGINE_FALLBACK_CPUS);
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
 * sched_getaffinity is reduced to at most LSM_SYS_CPUS cpus. This is a
 * real reduction of the returned mask (not a fabricated number), so a consumer
 * that counts set bits sees the workspace's cpu count. It cannot add cpus the
 * host does not have; it only drops the extras the host exposes.
 */
int sched_getaffinity(pid_t pid, size_t size, cpu_set_t *set) {
  static affinity_fn real;
  if (!real)
    real = (affinity_fn)shim_next("sched_getaffinity");
  int r = real(pid, size, set);
  if (r != 0)
    return r;
  long want = env_long("LSM_SYS_CPUS", ENGINE_FALLBACK_CPUS);
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
    real = (getauxval_fn)shim_next("getauxval");
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
    real = (confstr_fn)shim_next("confstr");
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
    real = (const char *(*)(void))shim_next("gnu_get_libc_version");
  static char trimmed[64];
  strip_distro_suffix(trimmed, real(), sizeof(trimmed));
  return trimmed;
}
