#!/usr/bin/env python3
"""Test-only nsjail stub.

Parses the rendered nsjail config that sandboxd hands to `nsjail --config` and
executes the requested binary directly (no isolation). Used solely to exercise
the end-to-end HTTP lifecycle on hosts where nsjail cannot be installed.

NOT part of the service. The service never invokes this; it is selected only
when the operator points `sandbox.nsjail_path` at it explicitly.
"""
import os
import re
import sys


def parse_config(path):
    text = open(path).read()
    # workspace mount src
    ws_root = None
    for m in re.finditer(r"mount\s*\{(.*?)\}", text, re.S):
        block = m.group(1)
        if 'dst: "/workspace"' in block:
            src = re.search(r'src:\s*"([^"]*)"', block)
            if src:
                ws_root = src.group(1)
    # exec_bin
    path_val = None
    args = []
    ex = re.search(r"exec_bin\s*\{(.*?)\}", text, re.S)
    if ex:
        block = ex.group(1)
        pm = re.search(r'path:\s*(?:"([^"]*)"|(\S+))', block)
        if pm:
            path_val = pm.group(1) if pm.group(1) is not None else pm.group(2)
        for am in re.finditer(r'arg:\s*(?:"([^"]*)"|(\S+))', block):
            args.append(am.group(1) if am.group(1) is not None else am.group(2))
    # envar passthrough
    env = {}
    for em in re.finditer(r'envar:\s*"([^"=]+)=(.*)"', text):
        env[em.group(1)] = em.group(2)
    return ws_root, path_val, args, env


def main():
    argv = sys.argv[1:]
    cfg = None
    for i, a in enumerate(argv):
        if a == "--config" and i + 1 < len(argv):
            cfg = argv[i + 1]
    if not cfg:
        sys.stderr.write("stub: missing --config\n")
        return 2
    ws_root, path_val, args, env = parse_config(cfg)
    if not path_val:
        sys.stderr.write("stub: no exec_bin path\n")
        return 2
    if ws_root:
        os.chdir(ws_root)
    full_env = dict(os.environ)
    full_env.update(env)
    try:
        os.execvpe(path_val, [path_val] + args, full_env)
    except FileNotFoundError:
        sys.stderr.write("stub: not found: %s\n" % path_val)
        return 127


if __name__ == "__main__":
    sys.exit(main())
