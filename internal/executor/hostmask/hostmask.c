/*
 * hostmask.c -- host-identity masking shim for sandboxd.
 *
 * WHAT THIS IS
 *   A small dynamically-linked shared object (a "preload shim") that is
 *   LD_PRELOADed (via /etc/ld.so.preload) into every dynamically-linked process
 *   a workspace runs. It does two jobs:
 *
 *   (1) SYSCALL MASK. Intercepts the syscall-reachable facts by which a jailed
 *       workload could identify the host it runs on, and answers with the
 *       WORKSPACE's limits instead:
 *
 *         uname(2)       release/version/nodename  -> a generic sandbox kernel
 *         sysconf(3)     _SC_NPROCESSORS_*          -> the workspace's cpu_cores
 *                        _SC_PHYS_PAGES/_SC_AVPHYS_PAGES -> the workspace's memory
 *         sysinfo(2)     totalram/freeram/procs     -> the workspace's memory/cpus
 *         sched_getaffinity(2) is reduced to the workspace's cpu_cores
 *         getauxval(3)   AT_HWCAP/AT_HWCAP2         -> 0 (no CPU fingerprint)
 *         confstr/gnu_get_libc_version -> trims the distro patch level
 *         dlsym(3) of a masked symbol (any handle)  -> the shim's own function
 *
 *   (2) FILESYSTEM HIDING. Answers ENOENT / hides the directory entry for a
 *       configured list of paths, so the masking mechanism itself is invisible
 *       to the tenant: /etc/ld.so.preload (which names this shim), this shim's
 *       own in-jail path, and the engine's legacy directories. The technique is
 *       an interposer on the filesystem entry points (open/stat/access/readdir/
 *       readlink/realpath). It hides a path from every process the shim is
 *       injected into, which is precisely the tenant's processes.
 *
 * WHY IT IS REACHED
 *   It is named in /etc/ld.so.preload, which the dynamic loader reads for EVERY
 *   dynamically-linked exec, independent of the LD_PRELOAD environment variable.
 *   A tenant therefore cannot disable it with `env -u LD_PRELOAD` or `env -i`
 *   (measured). The file and the shim are bind-mounted READ-ONLY by nsjail, so a
 *   tenant cannot replace them.
 *
 *   HOW THE HIDING DOES NOT BREAK THE LOADER. /etc/ld.so.preload has to stay
 *   readable by the DYNAMIC LOADER for the mask to work at all. The hiding is
 *   SCOPED to the shim itself: the loader that opens /etc/ld.so.preload runs
 *   BEFORE any preload object is mapped (it is the program that reads the list),
 *   so it is never intersected by the interposer; only a process that already
 *   has this shim loaded sees the file as ENOENT. A real exec is therefore still
 *   masked: `env -i uname -r` and `env -u LSM_SYS_* uname -r` still report the
 *   masked values (asserted by scripts/identity-check.sh).
 *
 * THE glibc VERSIONING TRAP (measured; this is the non-obvious part)
 *   glibc VERSIONS its symbols, and an unversioned definition in a preloaded
 *   object does not satisfy a versioned reference for symbols that exist only at
 *   a newer version. Measured: `ls`, `cat`, `find` were hidden immediately, but
 *   python's os.stat() and bash's test -f sailed through, because they import
 *   `fstatat64@GLIBC_2.33` and `stat64@GLIBC_2.33`, not the plain names. The fix
 *   is `.symver` aliases plus a linker version script (hostmask.map), emitting
 *   each filesystem interposer under the version the callers reference. The
 *   syscall-mask exports (uname/sysconf/...) reference their ancient GLIBC_2.2.5
 *   versions and keep working unversioned, so they are deliberately NOT listed.
 *
 * THE readlink -f / realpath DETAIL (measured; EACCES closes the echo)
 *   readlink(1) -f and realpath(1) canonicalize component by component and pass
 *   only the LEAF with a dirfd (`readlink(dirfd, "leaf", ...)`), so matching the
 *   full path alone misses them; the matcher therefore matches the final
 *   component too. They never stat the leaf, so there is no leaf stat to
 *   intercept -- but coreutils' canonicalizer DOES call readlink(2) on the leaf,
 *   and its behaviour depends on the errno: on ENOENT it treats the component as
 *   "not yet created" and echoes the literal argument (measured: `realpath
 *   /etc/ld.so.preload` printed the path), while on EACCES it aborts and prints
 *   nothing (measured: `realpath -e` and `readlink -f` exit 1). The canonical-
 *   ization family (readlink/readlinkat/realpath/canonicalize_file_name)
 *   therefore answers EACCES, not ENOENT, so the path is never echoed. The
 *   EXISTENCE family stays ENOENT so `ls`/`cat`/`find`/`test`/os.path.exists()
 *   still see the path as absent. See docs/DECISIONS.md ADR 81.
 *
 * ZERO SELF-ANNOUNCEMENT (measured)
 *   A tenant can `strings` this shim and read its own environment, so it must
 *   carry no self-announcing literal. The masked kernel values (0.0.0-sandbox,
 *   #1 SMP) and the configuration variable names (LSM_SYS_*) are stored XORed
 *   and decoded at runtime, so `strings` over the shim yields none of them; the
 *   hidden-path list is stored the same way. The only literals left are generic
 *   glibc names (open/stat/...) which name nothing about this engine.
 *
 * HONEST LIMITS (documented in docs/DECISIONS.md, do NOT remove)
 *   - A STATICALLY-LINKED binary does not run the dynamic loader at all, so it
 *     is not intercepted and sees the real host values / the real files.
 *   - A non-glibc C library (musl, e.g. an Alpine-built binary) is loaded by
 *     musl's own loader, which does not read /etc/ld.so.preload.
 *   - A direct syscall (syscall(2) with a raw uname/stat/open) bypasses libc.
 *   The first two are properties of preload-based interposition; the raw syscall
 *   is a property of the kernel ABI. The masking layer is defense-in-depth, not
 *   a security boundary. The /proc and /sys filesystem views remain hidden by
 *   the mount namespace, and those are what the acceptance suite asserts are
 *   unreadable.
 *
 * CONFIGURATION
 *   The syscall answers come from the environment; the names are deliberately
 *   opaque and engine-neutral (a tenant reads its own environment, so an
 *   engine-named variable would announce the engine). The names are stored
 *   XORed in this file and decoded at runtime:
 *     LSM_SYS_CPUS   decimal cpu count
 *     LSM_SYS_MEM    decimal bytes of memory
 *     LSM_SYS_REL    uname.release value
 *     LSM_SYS_VER    uname.version value
 *     LSM_SYS_NODE   uname.nodename value
 *   If a variable is missing the shim falls back to a conservative constant
 *   (1 cpu, 256 MB, generic kernel), so unsetting the environment cannot restore
 *   the host's values -- only a raw-syscall/static/musl bypass can.
 *
 *   The hidden-path list is built in: /etc/ld.so.preload, this shim's in-jail
 *   path, /sandboxd and /run/sandboxd. An operator may add up to four more via
 *   the generic HIDE_PATH/HIDE_PATH2..4 variables.
 *
 * REBUILD
 *   `make shim` rebuilds internal/executor/hostmask.so from this source and
 *   hostmask/hostmask.map. The Dockerfile rebuilds it from source at image
 *   build; the committed .so is the same source prebuilt so a plain
 *   `go build ./...` can embed it without a compiler.
 */
#define _GNU_SOURCE
#include <dirent.h>
#include <dlfcn.h>
#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <linux/stat.h>
#include <sched.h>
#include <stdarg.h>
#include <stdlib.h>
#include <string.h>
#include <sys/auxv.h>
#include <sys/stat.h>
#include <sys/sysinfo.h>
#include <sys/utsname.h>
#include <unistd.h>

#ifndef RTLD_NEXT
#define RTLD_NEXT ((void *)-1l)
#endif

/* Forward declarations of every syscall-mask function this shim interposes.
 * They are needed before the dlsym interposer, which hands their addresses back
 * to a caller. */
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
 * The shim therefore interposes `dlsym` itself: a request for one of the masked
 * symbols (a syscall-mask symbol OR a filesystem entry point) is answered with
 * the shim's own function, exactly the value the RTLD_DEFAULT path already
 * returns, for ANY handle. Every other lookup passes straight through, so no
 * program is disturbed. Measured to close CDLL("libc.so.6"),
 * CDLL("/lib/.../libc.so.6"), libc.dlopen+dlsym, dlsym(RTLD_NEXT,...),
 * dlmopen and the default handle (docs/VERIFICATION.md).
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
 * ───────────────────────── configuration decoding ────────────────────────
 * Every self-announcing literal is stored XORed with OBF_KEY so `strings` over
 * the shim yields none of: the masked kernel values, the LSM_SYS_* variable
 * names, or the hidden paths. decode() materialises them into static buffers
 * once, in init_state().
 */
#define OBF_KEY 0x5A

static const unsigned char OBF_ENV_CPUS[] = {0x16, 0x09, 0x17, 0x05, 0x09, 0x03, 0x09, 0x05, 0x19, 0x0a, 0x0f, 0x09, 0};
static const unsigned char OBF_ENV_MEM[] = {0x16, 0x09, 0x17, 0x05, 0x09, 0x03, 0x09, 0x05, 0x17, 0x1f, 0x17, 0};
static const unsigned char OBF_ENV_REL[] = {0x16, 0x09, 0x17, 0x05, 0x09, 0x03, 0x09, 0x05, 0x08, 0x1f, 0x16, 0};
static const unsigned char OBF_ENV_VER[] = {0x16, 0x09, 0x17, 0x05, 0x09, 0x03, 0x09, 0x05, 0x0c, 0x1f, 0x08, 0};
static const unsigned char OBF_ENV_NODE[] = {0x16, 0x09, 0x17, 0x05, 0x09, 0x03, 0x09, 0x05, 0x14, 0x15, 0x1e, 0x1f, 0};
static const unsigned char OBF_FB_REL[] = {0x6a, 0x74, 0x6a, 0x74, 0x6a, 0x77, 0x29, 0x3b, 0x34, 0x3e, 0x38, 0x35, 0x22, 0};
static const unsigned char OBF_FB_VER[] = {0x79, 0x6b, 0x7a, 0x09, 0x17, 0x0a, 0};
static const unsigned char OBF_FB_NODE[] = {0x29, 0x3b, 0x34, 0x3e, 0x38, 0x35, 0x22, 0};
static const unsigned char OBF_HIDE_PRELOAD[] = {0x75, 0x3f, 0x2e, 0x39, 0x75, 0x36, 0x3e, 0x74, 0x29, 0x35, 0x74, 0x2a, 0x28, 0x3f, 0x36, 0x35, 0x3b, 0x3e, 0};
static const unsigned char OBF_HIDE_SHIM[] = {0x75, 0x28, 0x2f, 0x34, 0x75, 0x74, 0x28, 0x2e, 0x74, 0x29, 0x35, 0};
static const unsigned char OBF_HIDE_DIR[] = {0x75, 0x29, 0x3b, 0x34, 0x3e, 0x38, 0x35, 0x22, 0x3e, 0};
static const unsigned char OBF_HIDE_RUNDIR[] = {0x75, 0x28, 0x2f, 0x34, 0x75, 0x29, 0x3b, 0x34, 0x3e, 0x38, 0x35, 0x22, 0x3e, 0};

/*
 * A harmless build marker kept as a live rodata literal so a `strings` audit can
 * tell THIS build from the previous unversioned artifact (the positive control
 * for "verify the binary you are testing"). It is a random hex token: it names
 * no engine, no platform, no kernel and no path, so it trips none of the
 * zero-signal assertions.
 */
static const char hostmask_build_marker[] __attribute__((used)) = "9f3a71c2";

static char cfg_env_cpus[16];
static char cfg_env_mem[16];
static char cfg_env_rel[16];
static char cfg_env_ver[16];
static char cfg_env_node[16];
static char cfg_fb_rel[32];
static char cfg_fb_ver[16];
static char cfg_fb_node[16];

static void decode(const unsigned char *obf, char *out, size_t n) {
  size_t i = 0;
  while (i + 1 < n && obf[i] != 0) {
    out[i] = (char)(obf[i] ^ OBF_KEY);
    i++;
  }
  out[i] = '\0';
}

/* ───────────────────────── hidden-path state ───────────────────────────── */
/*
 * The hidden-path list is a SINGLE explicit table so it is auditable. Each entry
 * is a path the masking mechanism (or a legacy engine artifact) would otherwise
 * announce to a tenant that browsed the filesystem. They are stored obfuscated
 * so `strings` over the shim yields none of them.
 *
 *   /etc/ld.so.preload -- the list that NAMES this shim; readable, so `cat`
 *                         would reveal both the mechanism and the shim path.
 *                         Still read by the dynamic loader before any preload
 *                         object is mapped, so a real exec stays masked.
 *   /run/.rt.so        -- this shim's own in-jail path.
 *   /sandboxd          -- the legacy engine directory at the filesystem root.
 *   /run/sandboxd      -- the legacy engine directory under /run.
 */
static const unsigned char *BUILTIN_HIDDEN[] = {
    OBF_HIDE_PRELOAD,
    OBF_HIDE_SHIM,
    OBF_HIDE_DIR,
    OBF_HIDE_RUNDIR,
};
#define BUILTIN_HIDDEN_N (sizeof(BUILTIN_HIDDEN) / sizeof(BUILTIN_HIDDEN[0]))

/* Six is enough for the built-ins plus the operator's four optional HIDE_PATH*
 * variables; a table rather than a hard-coded path keeps it testable. */
#define MAX_HIDDEN 8
static char hidden[MAX_HIDDEN][PATH_MAX];
static int hidden_n;
static int state_done;

static void add_hidden(const char *p) {
  if (!p || !*p || hidden_n >= MAX_HIDDEN)
    return;
  size_t n = strlen(p);
  if (n >= PATH_MAX)
    return;
  memcpy(hidden[hidden_n], p, n + 1);
  hidden_n++;
}

static void add_hidden_obf(const unsigned char *obf) {
  char buf[PATH_MAX];
  decode(obf, buf, sizeof(buf));
  add_hidden(buf);
}

static void init_state(void) {
  if (state_done)
    return;
  state_done = 1;
  decode(OBF_ENV_CPUS, cfg_env_cpus, sizeof(cfg_env_cpus));
  decode(OBF_ENV_MEM, cfg_env_mem, sizeof(cfg_env_mem));
  decode(OBF_ENV_REL, cfg_env_rel, sizeof(cfg_env_rel));
  decode(OBF_ENV_VER, cfg_env_ver, sizeof(cfg_env_ver));
  decode(OBF_ENV_NODE, cfg_env_node, sizeof(cfg_env_node));
  decode(OBF_FB_REL, cfg_fb_rel, sizeof(cfg_fb_rel));
  decode(OBF_FB_VER, cfg_fb_ver, sizeof(cfg_fb_ver));
  decode(OBF_FB_NODE, cfg_fb_node, sizeof(cfg_fb_node));
  for (size_t i = 0; i < BUILTIN_HIDDEN_N; i++)
    add_hidden_obf(BUILTIN_HIDDEN[i]);
  /* Operator-configured extra paths. The names are generic (they name no
   * engine). */
  const char *keys[] = {"HIDE_PATH", "HIDE_PATH2", "HIDE_PATH3", "HIDE_PATH4"};
  for (size_t i = 0; i < sizeof(keys) / sizeof(keys[0]); i++) {
    const char *v = getenv(keys[i]);
    if (v && *v)
      add_hidden(v);
  }
}

static const char *base_of(const char *p) {
  const char *b = strrchr(p, '/');
  return b ? b + 1 : p;
}

/* path_has_prefix reports whether p is prefix itself or a child of it. */
static int path_has_prefix(const char *p, const char *prefix) {
  size_t n = strlen(prefix);
  if (strncmp(p, prefix, n) != 0)
    return 0;
  return p[n] == '\0' || p[n] == '/';
}

static int is_hidden(const char *p) {
  if (!p)
    return 0;
  init_state();
  for (int i = 0; i < hidden_n; i++) {
    if (!strcmp(p, hidden[i]))
      return 1;
    if (path_has_prefix(p, hidden[i]))
      return 1;
    /* A relative path (dirfd + leaf) will not equal the absolute hidden path,
     * so match the final component too: readlink(1)/realpath(1) canonicalize
     * component by component and only ever pass the leaf. */
    if (!strcmp(base_of(p), base_of(hidden[i])))
      return 1;
  }
  return 0;
}

static int is_hidden_name(const char *n) {
  if (!n)
    return 0;
  init_state();
  for (int i = 0; i < hidden_n; i++) {
    if (!strcmp(n, base_of(hidden[i])))
      return 1;
  }
  return 0;
}

#define MISSING() do { errno = ENOENT; return -1; } while (0)

/*
 * THE CANONICALIZATION FAMILY ANSWERS EACCES, NOT ENOENT (measured; this is the
 * fix for the "readlink -f / realpath still echo the hidden path" regression).
 *
 *   Coreutils' realpath(1)/readlink -f and Python's os.path.realpath() do NOT
 *   stat the leaf with a syscall we can answer: they canonicalize the path with
 *   readlink(2)/lstat(2) component by component. Measured (strace, coreutils
 *   9.4), on a path whose final component the kernel reports ENOENT:
 *
 *       readlink("/etc", ...)               = -1 EINVAL (not a symlink)
 *       readlink("/etc/ld.so.preload", ...) = -1 ENOENT
 *       -> stdout: /etc/ld.so.preload          (the literal ARGUMENT, echoed)
 *       -> exit 0
 *
 *   An ENOENT from the canonicalizer's readlink() is interpreted by coreutils as
 *   "this component does not exist yet", so it KEEPS the literal it was given
 *   and prints it -- an ENOENT cannot hide the path. Measured: the same path
 *   under EACCES instead aborts:
 *
 *       readlink("/etc/ld.so.preload", ...) = -1 EACCES
 *       -> stderr: realpath: /etc/ld.so.preload: Permission denied
 *       -> exit 1 (no stdout)
 *
 *   EACCES is therefore the errno that closes the echo, and it is the natural
 *   answer for a path whose components exist but whose leaf is withheld. The
 *   EXISTENCE family (open/stat/lstat/fstatat/statx/access/readdir) still
 *   answers ENOENT, so human tools (ls/cat/find/test) and Python's
 *   os.path.exists()/open()/listdir()/glob() continue to see the path as simply
 *   absent -- the measured, working hide. Only the canonicalization family (the
 *   tools that would otherwise echo the literal) answers EACCES.
 *
 *   RESIDUAL (documented, not closed): Python's os.path.realpath() swallows the
 *   OSError and returns the input unchanged, so it still echoes the tenant's own
 *   argument. It reveals nothing the tenant did not type, and os.path.exists()
 *   correctly reports False. A raw syscall(2) readlink bypasses this too, like
 *   every other interposer.
 */
#define DENIED() do { errno = EACCES; return -1; } while (0)
#define DENIED_NULL() do { errno = EACCES; return NULL; } while (0)

/* ───────────────────────── filesystem interposers ──────────────────────── */
/* These are exported under the version names their callers reference; the
 * .symver block at the bottom of this file builds that mapping and
 * hostmask.map supplies the version nodes. */

int open_v(const char *path, int flags, ...) {
  static int (*real)(const char *, int, ...);
  if (!real)
    real = (int (*)(const char *, int, ...))shim_next("open");
  if (is_hidden(path))
    MISSING();
  if (flags & O_CREAT) {
    va_list a;
    va_start(a, flags);
    mode_t m = va_arg(a, mode_t);
    va_end(a);
    return real(path, flags, m);
  }
  return real(path, flags);
}
int open64_v(const char *path, int flags, ...) {
  static int (*real)(const char *, int, ...);
  if (!real)
    real = (int (*)(const char *, int, ...))shim_next("open64");
  if (is_hidden(path))
    MISSING();
  if (flags & O_CREAT) {
    va_list a;
    va_start(a, flags);
    mode_t m = va_arg(a, mode_t);
    va_end(a);
    return real(path, flags, m);
  }
  return real(path, flags);
}
int openat_v(int dirfd, const char *path, int flags, ...) {
  static int (*real)(int, const char *, int, ...);
  if (!real)
    real = (int (*)(int, const char *, int, ...))shim_next("openat");
  if (is_hidden(path))
    MISSING();
  if (flags & O_CREAT) {
    va_list a;
    va_start(a, flags);
    mode_t m = va_arg(a, mode_t);
    va_end(a);
    return real(dirfd, path, flags, m);
  }
  return real(dirfd, path, flags);
}
int openat64_v(int dirfd, const char *path, int flags, ...) {
  static int (*real)(int, const char *, int, ...);
  if (!real)
    real = (int (*)(int, const char *, int, ...))shim_next("openat64");
  if (is_hidden(path))
    MISSING();
  if (flags & O_CREAT) {
    va_list a;
    va_start(a, flags);
    mode_t m = va_arg(a, mode_t);
    va_end(a);
    return real(dirfd, path, flags, m);
  }
  return real(dirfd, path, flags);
}

int stat_v(const char *path, struct stat *b) {
  static int (*real)(const char *, struct stat *);
  if (!real)
    real = (int (*)(const char *, struct stat *))shim_next("stat");
  if (is_hidden(path))
    MISSING();
  return real(path, b);
}
int lstat_v(const char *path, struct stat *b) {
  static int (*real)(const char *, struct stat *);
  if (!real)
    real = (int (*)(const char *, struct stat *))shim_next("lstat");
  if (is_hidden(path))
    MISSING();
  return real(path, b);
}
int fstatat_v(int dirfd, const char *path, struct stat *b, int flags) {
  static int (*real)(int, const char *, struct stat *, int);
  if (!real)
    real = (int (*)(int, const char *, struct stat *, int))shim_next("fstatat");
  if (is_hidden(path))
    MISSING();
  return real(dirfd, path, b, flags);
}
int fstatat64_v(int dirfd, const char *path, struct stat *b, int flags) {
  static int (*real)(int, const char *, struct stat *, int);
  if (!real)
    real = (int (*)(int, const char *, struct stat *, int))shim_next("fstatat64");
  if (is_hidden(path))
    MISSING();
  return real(dirfd, path, b, flags);
}
int statx_v(int dirfd, const char *path, int flags, unsigned int mask, struct statx *b) {
  static int (*real)(int, const char *, int, unsigned int, struct statx *);
  if (!real)
    real = (int (*)(int, const char *, int, unsigned int, struct statx *))shim_next("statx");
  if (is_hidden(path))
    MISSING();
  return real(dirfd, path, flags, mask, b);
}
int __xstat_v(int ver, const char *path, struct stat *b) {
  (void)ver;
  return stat_v(path, b);
}
int __lxstat_v(int ver, const char *path, struct stat *b) {
  (void)ver;
  return lstat_v(path, b);
}
int __fxstatat_v(int ver, int dirfd, const char *path, struct stat *b, int flags) {
  (void)ver;
  return fstatat_v(dirfd, path, b, flags);
}
int stat64_v(const char *path, struct stat *b) { return stat_v(path, b); }
int lstat64_v(const char *path, struct stat *b) { return lstat_v(path, b); }
int fstat64_v(int fd, struct stat *b) {
  static int (*real)(int, struct stat *);
  if (!real)
    real = (int (*)(int, struct stat *))shim_next("fstat64");
  return real(fd, b);
}

int access_v(const char *path, int mode) {
  static int (*real)(const char *, int);
  if (!real)
    real = (int (*)(const char *, int))shim_next("access");
  if (is_hidden(path))
    MISSING();
  return real(path, mode);
}
int faccessat_v(int dirfd, const char *path, int mode, int flags) {
  static int (*real)(int, const char *, int, int);
  if (!real)
    real = (int (*)(int, const char *, int, int))shim_next("faccessat");
  if (is_hidden(path))
    MISSING();
  return real(dirfd, path, mode, flags);
}
/* eaccess is an alias of euidaccess (same libc address); coreutils `test -r`
 * references euidaccess@GLIBC_2.2.5, so both open names must be bound or
 * `test -r` sails through while `test -e`/`-f` are hidden (measured). */
int eaccess_v(const char *path, int mode) { return access_v(path, mode); }
int euidaccess_v(const char *path, int mode) { return access_v(path, mode); }

ssize_t readlink_v(const char *path, char *buf, size_t n) {
  static ssize_t (*real)(const char *, char *, size_t);
  if (!real)
    real = (ssize_t (*)(const char *, char *, size_t))shim_next("readlink");
  if (is_hidden(path))
    DENIED();
  return real(path, buf, n);
}
ssize_t readlinkat_v(int dirfd, const char *path, char *buf, size_t n) {
  static ssize_t (*real)(int, const char *, char *, size_t);
  if (!real)
    real = (ssize_t (*)(int, const char *, char *, size_t))shim_next("readlinkat");
  if (is_hidden(path))
    DENIED();
  return real(dirfd, path, buf, n);
}

struct dirent *readdir_v(DIR *d) {
  static struct dirent *(*real)(DIR *);
  if (!real)
    real = (struct dirent * (*)(DIR *)) shim_next("readdir");
  struct dirent *e;
  while ((e = real(d)) && is_hidden_name(e->d_name))
    ;
  return e;
}
struct dirent64 *readdir64_v(DIR *d) {
  static struct dirent64 *(*real)(DIR *);
  if (!real)
    real = (struct dirent64 * (*)(DIR *)) shim_next("readdir64");
  struct dirent64 *e;
  while ((e = real(d)) && is_hidden_name(e->d_name))
    ;
  return e;
}
int readdir_r_v(DIR *d, struct dirent *e, struct dirent **r) {
  static int (*real)(DIR *, struct dirent *, struct dirent **);
  if (!real)
    real = (int (*)(DIR *, struct dirent *, struct dirent **))shim_next("readdir_r");
  int rc;
  while ((rc = real(d, e, r)) == 0 && *r && is_hidden_name((*r)->d_name))
    ;
  return rc;
}

char *realpath_v(const char *path, char *resolved) {
  static char *(*real)(const char *, char *);
  if (!real)
    real = (char *(*)(const char *, char *))shim_next("realpath");
  if (is_hidden(path))
    DENIED_NULL();
  return real(path, resolved);
}
char *canonicalize_file_name_v(const char *path) {
  static char *(*real)(const char *);
  if (!real)
    real = (char *(*)(const char *))shim_next("canonicalize_file_name");
  if (is_hidden(path))
    DENIED_NULL();
  return real(path);
}

/*
 * is_masked_symbol reports whether a looked-up symbol's answer the shim owns.
 * It includes `dlsym` itself so a handle-scoped lookup of the loader API also
 * resolves to the shim rather than stepping behind it, and every filesystem
 * entry point so a handle-scoped lookup from libc cannot step behind the hiding.
 */
static int is_masked_symbol(const char *s) {
  if (!s)
    return 0;
  static const char *names[] = {
      "uname", "sysconf", "sysinfo", "sched_getaffinity", "getauxval",
      "confstr", "gnu_get_libc_version", "dlsym",
      "open", "open64", "openat", "openat64",
      "stat", "stat64", "lstat", "lstat64", "fstat", "fstat64",
      "fstatat", "fstatat64", "statx",
      "__xstat", "__lxstat", "__fxstatat",
      "access", "faccessat", "eaccess", "euidaccess",
      "readlink", "readlinkat", "readdir", "readdir64", "readdir_r",
      "realpath", "canonicalize_file_name",
  };
  for (size_t i = 0; i < sizeof(names) / sizeof(names[0]); i++)
    if (!strcmp(s, names[i]))
      return 1;
  return 0;
}

static void *masked_symbol_addr(const char *symbol) {
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
  if (!strcmp(symbol, "dlsym"))
    return (void *)&dlsym;
  if (!strcmp(symbol, "open"))
    return (void *)&open_v;
  if (!strcmp(symbol, "open64"))
    return (void *)&open64_v;
  if (!strcmp(symbol, "openat"))
    return (void *)&openat_v;
  if (!strcmp(symbol, "openat64"))
    return (void *)&openat64_v;
  if (!strcmp(symbol, "stat"))
    return (void *)&stat_v;
  if (!strcmp(symbol, "stat64"))
    return (void *)&stat64_v;
  if (!strcmp(symbol, "lstat"))
    return (void *)&lstat_v;
  if (!strcmp(symbol, "lstat64"))
    return (void *)&lstat64_v;
  if (!strcmp(symbol, "fstat"))
    return (void *)&fstat64_v; /* same behaviour for the fd-based calls */
  if (!strcmp(symbol, "fstat64"))
    return (void *)&fstat64_v;
  if (!strcmp(symbol, "fstatat"))
    return (void *)&fstatat_v;
  if (!strcmp(symbol, "fstatat64"))
    return (void *)&fstatat64_v;
  if (!strcmp(symbol, "statx"))
    return (void *)&statx_v;
  if (!strcmp(symbol, "__xstat"))
    return (void *)&__xstat_v;
  if (!strcmp(symbol, "__lxstat"))
    return (void *)&__lxstat_v;
  if (!strcmp(symbol, "__fxstatat"))
    return (void *)&__fxstatat_v;
  if (!strcmp(symbol, "access"))
    return (void *)&access_v;
  if (!strcmp(symbol, "faccessat"))
    return (void *)&faccessat_v;
  if (!strcmp(symbol, "eaccess"))
    return (void *)&eaccess_v;
  if (!strcmp(symbol, "euidaccess"))
    return (void *)&euidaccess_v;
  if (!strcmp(symbol, "readlink"))
    return (void *)&readlink_v;
  if (!strcmp(symbol, "readlinkat"))
    return (void *)&readlinkat_v;
  if (!strcmp(symbol, "readdir"))
    return (void *)&readdir_v;
  if (!strcmp(symbol, "readdir64"))
    return (void *)&readdir64_v;
  if (!strcmp(symbol, "readdir_r"))
    return (void *)&readdir_r_v;
  if (!strcmp(symbol, "realpath"))
    return (void *)&realpath_v;
  if (!strcmp(symbol, "canonicalize_file_name"))
    return (void *)&canonicalize_file_name_v;
  return NULL;
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
  if (is_masked_symbol(symbol))
    return masked_symbol_addr(symbol);
  if (!real_dlsym)
    real_dlsym = (dlsym_fn)dlvsym(RTLD_NEXT, "dlsym", "GLIBC_2.2.5");
  return real_dlsym(handle, symbol);
}

/* ───────────────────────── syscall mask ────────────────────────────────── */

/* Conservative fallbacks. They are decoded from OBF_FB_* so `strings` over the
 * shim yields no kernel value. They match the defaults in internal/config
 * (DefaultMaskKernelRelease/Version) so `env -i` -- which clears the LSM_SYS_*
 * variables -- still answers with the same generic values. */
#define ENGINE_FALLBACK_CPUS 1L
#define ENGINE_FALLBACK_MEM (256L * 1024 * 1024)

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
    init_state();
    copy_field(buf->release, sizeof(buf->release),
               env_str(cfg_env_rel, cfg_fb_rel));
    copy_field(buf->version, sizeof(buf->version),
               env_str(cfg_env_ver, cfg_fb_ver));
    copy_field(buf->nodename, sizeof(buf->nodename),
               env_str(cfg_env_node, cfg_fb_node));
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
    init_state();
    return env_long(cfg_env_cpus, ENGINE_FALLBACK_CPUS);
  case _SC_PHYS_PAGES:
  case _SC_AVPHYS_PAGES: {
    init_state();
    long page = real(_SC_PAGE_SIZE);
    if (page <= 0)
      page = 4096;
    return env_long(cfg_env_mem, ENGINE_FALLBACK_MEM) / page;
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
    init_state();
    long mem = env_long(cfg_env_mem, ENGINE_FALLBACK_MEM);
    long cpus = env_long(cfg_env_cpus, ENGINE_FALLBACK_CPUS);
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
  init_state();
  long want = env_long(cfg_env_cpus, ENGINE_FALLBACK_CPUS);
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

/* ───────────────────────── version bindings ────────────────────────────── */
/*
 * This is the part the first attempt missed. Each filesystem interposer is
 * emitted under the version name its callers actually reference; without it an
 * unversioned definition does not satisfy a versioned reference on this glibc,
 * so python (fstatat64@GLIBC_2.33) and bash's `test -f` (stat64@GLIBC_2.33)
 * sailed through. The syscall-mask exports are left unversioned on purpose:
 * their references are the ancient GLIBC_2.2.5 versions and they keep binding.
 */
__asm__(".symver open_v,open@GLIBC_2.2.5");
__asm__(".symver open64_v,open64@GLIBC_2.2.5");
__asm__(".symver openat_v,openat@GLIBC_2.4");
__asm__(".symver openat64_v,openat64@GLIBC_2.4");
__asm__(".symver stat_v,stat@GLIBC_2.33");
__asm__(".symver lstat_v,lstat@GLIBC_2.33");
__asm__(".symver fstatat_v,fstatat@GLIBC_2.33");
__asm__(".symver fstatat64_v,fstatat64@GLIBC_2.33");
__asm__(".symver stat64_v,stat64@GLIBC_2.33");
__asm__(".symver lstat64_v,lstat64@GLIBC_2.33");
__asm__(".symver fstat64_v,fstat64@GLIBC_2.33");
__asm__(".symver statx_v,statx@GLIBC_2.28");
__asm__(".symver __xstat_v,__xstat@GLIBC_2.2.5");
__asm__(".symver __lxstat_v,__lxstat@GLIBC_2.2.5");
__asm__(".symver __fxstatat_v,__fxstatat@GLIBC_2.4");
__asm__(".symver access_v,access@GLIBC_2.2.5");
__asm__(".symver faccessat_v,faccessat@GLIBC_2.4");
__asm__(".symver eaccess_v,eaccess@GLIBC_2.4");
__asm__(".symver euidaccess_v,euidaccess@GLIBC_2.2.5");
__asm__(".symver readlink_v,readlink@GLIBC_2.2.5");
__asm__(".symver readlinkat_v,readlinkat@GLIBC_2.4");
__asm__(".symver readdir_v,readdir@GLIBC_2.2.5");
__asm__(".symver readdir64_v,readdir64@GLIBC_2.2.5");
__asm__(".symver readdir_r_v,readdir_r@GLIBC_2.2.5");
__asm__(".symver realpath_v,realpath@GLIBC_2.3");
__asm__(".symver canonicalize_file_name_v,canonicalize_file_name@GLIBC_2.2.5");
