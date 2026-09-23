#!/usr/bin/python3
"""RunnerLoom agent-task runner.

Runs as root inside a disposable guest VM. The coding agent runs as the
unprivileged runner user, without sudo unless its profile asks for it. The
GitHub token never enters a runner-owned process after the agent starts: the
agent's changes are committed as runner without secrets, every runner process
is killed, and root then pushes from a separate clean repository.

Everything printed here goes to the serial console, which the Node keeps as a
bounded owner-only log. Agent output is mirrored with a "| " prefix so it can
never look like a result marker. The result is a chunked, SHA-256 checked JSON
document, emitted more than once so that a Node agent restart cannot lose it.

The RUNNERLOOM_TASK_* environment variables exist only so the repository tests
can exercise this file without a VM. cloud-init never sets them.
"""
import base64
import collections
import glob
import hashlib
import json
import os
import pwd
import re
import shutil
import signal
import subprocess
import sys
import tarfile
import tempfile
import termios
import threading
import time
import urllib.error
import urllib.parse
import urllib.request

PAYLOAD = os.environ.get('RUNNERLOOM_TASK_PAYLOAD', '/run/runnerloom-task.json')
USER = os.environ.get('RUNNERLOOM_TASK_USER', 'runner')
HOME = os.environ.get('RUNNERLOOM_TASK_HOME', '/home/runner')
STATE = os.environ.get('RUNNERLOOM_TASK_STATE', '/var/lib/runnerloom-task')
POWEROFF = os.environ.get('RUNNERLOOM_TASK_POWEROFF', '1') == '1'
EMIT_REPEAT = int(os.environ.get('RUNNERLOOM_TASK_EMIT_REPEAT', '3'))
CONTROL = os.path.join(HOME, '.runnerloom')
HELPER = os.path.join(STATE, 'git-credential')
CLEAN = os.path.join(STATE, 'push')
SYSTEM_PATH = '/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'
OUTPUT_LIMIT = 60 << 10
PATCH_LIMIT = 384 << 10
DIFFSTAT_LIMIT = 16 << 10
# The Node keeps 16 MiB of serial output per VM. Mirrored agent output stops
# well before that, leaving room for boot messages and repeated results.
MIRROR_LIMIT = 10 << 20
MIRROR_LINE = 2000
CHUNK = 512
# The host stops the VM at the Controller deadline. The runner finishes a
# margin earlier, and the agent a further reserve earlier so that commit,
# push, pull request and result output always fit.
GUEST_MARGIN = 120
POST_AGENT_RESERVE = 300
HELPER_SCRIPT = '''#!/bin/sh
[ "$1" = get ] || exit 0
proto= host=
while IFS='=' read -r key value; do
  [ -n "$key" ] || break
  case "$key" in protocol) proto=$value;; host) host=$value;; esac
done
[ "$proto" = https ] && [ -n "$RUNNERLOOM_GIT_HOST" ] && [ "$host" = "$RUNNERLOOM_GIT_HOST" ] && [ -n "$RUNNERLOOM_GIT_TOKEN" ] || exit 0
printf 'username=x-access-token\\npassword=%s\\n' "$RUNNERLOOM_GIT_TOKEN"
'''


class StepError(Exception):
    pass


_out = threading.Lock()


def line(text):
    # One write per line under a lock: result chunks can never be split.
    with _out:
        sys.stdout.write(text + '\n')
        sys.stdout.flush()


def status(message):
    line('RUNNERLOOM_TASK ' + message)


def tail(text, limit):
    data = text.encode('utf-8', 'replace')
    if len(data) <= limit:
        return text, False
    return data[-limit:].decode('utf-8', 'ignore'), True


class Clock:
    def __init__(self, deadline_unix):
        self.end = deadline_unix - GUEST_MARGIN
        self.agent_end = self.end - POST_AGENT_RESERVE

    def budget(self, cap, until=None):
        left = (until or self.end) - time.time()
        if left < 5:
            raise StepError('task time limit reached')
        return min(cap, left)


ACCOUNT = None


def account():
    if not USER:
        return None
    entry = pwd.getpwnam(USER)
    return entry.pw_uid, entry.pw_gid


def demote():
    if ACCOUNT is None:
        return {}
    return {'user': ACCOUNT[0], 'group': ACCOUNT[1], 'extra_groups': []}


def chown(path):
    if ACCOUNT is not None:
        os.chown(path, ACCOUNT[0], ACCOUNT[1], follow_symlinks=False)


def run(argv, env, cwd=None, as_user=True, timeout=600, check=True):
    kwargs = demote() if as_user else {}
    try:
        done = subprocess.run(argv, env=env, cwd=cwd, timeout=timeout, stdin=subprocess.DEVNULL,
                              stdout=subprocess.PIPE, stderr=subprocess.STDOUT, **kwargs)
    except subprocess.TimeoutExpired:
        raise StepError('%s timed out' % argv[0])
    output = done.stdout.decode('utf-8', 'replace')
    if check and done.returncode != 0:
        raise StepError('%s %s failed (%d): %s' % (argv[0], argv[1] if len(argv) > 1 else '', done.returncode, tail(output, 2000)[0]))
    return done.returncode, output


def retry(what, fn, attempts=3):
    for attempt in range(attempts):
        try:
            return fn()
        except StepError as error:
            if attempt == attempts - 1:
                raise
            status('%s failed, retrying: %s' % (what, str(error)[:200]))
            time.sleep(5 * (attempt + 1))


def kill_user_processes(group=None):
    """Stop everything the agent left behind before secrets are used again."""
    if ACCOUNT is None:
        if group:
            try:
                os.killpg(group, signal.SIGKILL)
            except (ProcessLookupError, PermissionError):
                pass
        return
    for _ in range(40):
        subprocess.run(['pkill', '-KILL', '-u', USER], check=False)
        if subprocess.run(['pgrep', '-u', USER], stdout=subprocess.DEVNULL, check=False).returncode != 0:
            return
        time.sleep(0.25)
    raise StepError('could not stop the agent user processes')


def private_dir(path, owner=True):
    os.makedirs(path, mode=0o700, exist_ok=True)
    os.chmod(path, 0o700)
    if owner:
        chown(path)


def place_files(files):
    home = os.path.realpath(HOME)
    for item in files:
        relative = item['path']
        parts = relative.split('/')
        if relative.startswith('/') or any(p in ('', '.', '..') for p in parts):
            raise StepError('invalid credential path')
        current = home
        for part in parts[:-1]:
            current = os.path.join(current, part)
            if os.path.islink(current):
                raise StepError('credential directory is a symlink')
            if not os.path.isdir(current):
                private_dir(current)
        target = os.path.join(current, parts[-1])
        fd = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, 0o600)
        try:
            os.write(fd, base64.b64decode(item.get('content') or ''))
            os.fchmod(fd, 0o600)
            if ACCOUNT is not None:
                os.fchown(fd, ACCOUNT[0], ACCOUNT[1])
        finally:
            os.close(fd)


def shred_seed_copies():
    """cloud-init keeps the user-data (with every credential) for root. Remove
    the copies before the agent starts; the agent cannot read them without
    sudo, and a sudo-enabled profile is documented as seeing everything."""
    if ACCOUNT is None:
        return
    for pattern in ('/var/lib/cloud/instances/*/user-data.txt*', '/var/lib/cloud/instances/*/cloud-config.txt',
                    '/var/lib/cloud/instances/*/obj.pkl'):
        for path in glob.glob(pattern):
            try:
                os.remove(path)
            except OSError:
                pass


def fetch(url, timeout=120):
    request = urllib.request.Request(url, headers={'User-Agent': 'runnerloom-task'})
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            return response.read()
    except (OSError, urllib.error.URLError) as error:
        raise StepError('download failed: %s' % error)


def ensure_node(env, clock):
    node = shutil.which('node', path=env['PATH'])
    if node:
        _, version = run([node, '--version'], env, as_user=False, check=False, timeout=30)
        match = re.match(r'v(\d+)\.', version.strip())
        if match and int(match.group(1)) >= 22:
            return
    status('installing Node.js 22 (SHA-256 checked against nodejs.org SHASUMS256.txt)')
    base = 'https://nodejs.org/dist/latest-v22.x/'
    sums = retry('Node.js index', lambda: fetch(base + 'SHASUMS256.txt', timeout=clock.budget(120))).decode()
    match = re.search(r'^([a-f0-9]{64})\s+(node-v22\.\d+\.\d+-linux-x64\.tar\.xz)$', sums, re.M)
    if not match:
        raise StepError('Node.js release index had no linux-x64 archive')
    archive = retry('Node.js download', lambda: fetch(base + match.group(2), timeout=clock.budget(900)))
    if hashlib.sha256(archive).hexdigest() != match.group(1):
        raise StepError('Node.js archive digest mismatch')
    with tempfile.TemporaryDirectory() as tmp:
        path = os.path.join(tmp, 'node.tar.xz')
        with open(path, 'wb') as f:
            f.write(archive)
        with tarfile.open(path, 'r:xz') as tar:
            try:
                tar.extractall(tmp, filter='data')
            except TypeError:
                tar.extractall(tmp)
        root = os.path.join(tmp, match.group(2)[:-len('.tar.xz')])
        for name in ('bin', 'include', 'lib', 'share'):
            source = os.path.join(root, name)
            if os.path.isdir(source):
                shutil.copytree(source, os.path.join('/usr/local', name), symlinks=True, dirs_exist_ok=True)


def github_api(method, path, token, body=None, timeout=60):
    data = json.dumps(body).encode() if body is not None else None
    request = urllib.request.Request('https://api.github.com' + path, data=data, method=method, headers={
        'Authorization': 'Bearer ' + token, 'Accept': 'application/vnd.github+json',
        'X-GitHub-Api-Version': '2022-11-28', 'User-Agent': 'runnerloom-task', 'Content-Type': 'application/json'})
    with urllib.request.urlopen(request, timeout=timeout) as response:
        return json.loads(response.read().decode() or 'null')


def pull_request(spec, task_id, title, default_branch, clock):
    token = spec['gitToken']
    parsed = urllib.parse.urlparse(spec['repository'])
    owner, repo = parsed.path.strip('/').removesuffix('.git').split('/')
    quoted = '/repos/%s/%s' % (urllib.parse.quote(owner), urllib.parse.quote(repo))
    base = spec.get('baseRef') or default_branch
    # The prompt stays private: a pull request on a public repository is public.
    body = 'Created by RunnerLoom task `%s` (agent: `%s`).' % (task_id, spec['agent']['name'])
    try:
        return github_api('POST', quoted + '/pulls', token, {'title': title, 'head': spec['branch'], 'base': base, 'body': body, 'draft': True}, timeout=clock.budget(60))['html_url']
    except urllib.error.HTTPError as error:
        if error.code != 422:
            raise
    query = urllib.parse.urlencode({'head': owner + ':' + spec['branch'], 'state': 'open'})
    found = github_api('GET', quoted + '/pulls?' + query, token, timeout=clock.budget(60))
    if found:
        return found[0]['html_url']
    raise StepError('pull request could not be created or found')


class Mirror:
    def __init__(self):
        self.buffer = collections.deque()
        self.size = 0
        self.mirrored = 0
        self.stopped = False

    def add(self, text):
        if not self.stopped and self.mirrored < MIRROR_LIMIT:
            shown = text[:MIRROR_LINE]
            self.mirrored += len(shown.encode('utf-8', 'replace')) + 4
            line('| ' + shown)
        self.buffer.append(text)
        self.size += len(text) + 1
        while self.size > OUTPUT_LIMIT * 2 and self.buffer:
            self.size -= len(self.buffer.popleft()) + 1


def run_agent(argv, env, cwd, clock, mirror):
    process = subprocess.Popen(argv, env=env, cwd=cwd, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                               stderr=subprocess.STDOUT, start_new_session=True, **demote())

    def read():
        for raw in iter(process.stdout.readline, b''):
            mirror.add(raw.decode('utf-8', 'replace').rstrip('\r\n'))

    reader = threading.Thread(target=read, daemon=True)
    reader.start()
    timed_out = False
    try:
        process.wait(timeout=max(1, clock.agent_end - time.time()))
    except subprocess.TimeoutExpired:
        timed_out = True
        try:
            os.killpg(process.pid, signal.SIGTERM)
            process.wait(timeout=30)
        except (subprocess.TimeoutExpired, ProcessLookupError):
            pass
    # Leftover children would keep writing into the pipe and the work tree.
    kill_user_processes(process.pid)
    process.wait()
    reader.join(timeout=30)
    output, truncated = tail('\n'.join(mirror.buffer), OUTPUT_LIMIT)
    return (124 if timed_out else process.returncode), output, truncated, timed_out


def emit(result, mirror):
    mirror.stopped = True
    data = json.dumps(result, ensure_ascii=False, separators=(',', ':')).encode('utf-8')
    encoded = base64.b64encode(data).decode()
    chunks = [encoded[i:i + CHUNK] for i in range(0, len(encoded), CHUNK)] or ['']
    digest = hashlib.sha256(data).hexdigest()
    for round_ in range(max(1, EMIT_REPEAT)):
        if round_:
            time.sleep(3)
        for index, chunk in enumerate(chunks):
            line('RUNNERLOOM_TASK_RESULT %d %s' % (index, chunk))
        line('RUNNERLOOM_TASK_RESULT_END %d %s' % (len(chunks), digest))


def main():
    global ACCOUNT
    os.umask(0o022)
    result = {'status': 'failed', 'exitCode': -1, 'changed': False, 'pushed': False}
    mirror = Mirror()
    try:
        with open(PAYLOAD, encoding='utf-8') as f:
            document = json.load(f)
        try:
            os.remove(PAYLOAD)
        except OSError:
            pass
        spec = document['spec']
        task_id = document['id']
        title = document.get('title') or 'RunnerLoom task'
        clock = Clock(int(document['deadlineUnix']))
        ACCOUNT = account()
        shred_seed_copies()
        status('started task %s agent=%s' % (task_id, spec['agent']['name']))
        work = os.path.join(HOME, 'work')
        if ACCOUNT is not None:
            subprocess.run(['dmesg', '-n', '1'], check=False)
            # systemd's console status lines (and their \r erasures) would
            # otherwise land inside result lines. The result is still printed
            # more than once, because other writers cannot all be silenced.
            try:
                os.kill(1, signal.SIGRTMIN + 21)
            except OSError:
                pass
            if os.path.exists('/dev/vdb'):
                run(['mkfs.ext4', '-q', '/dev/vdb'], {'PATH': SYSTEM_PATH}, as_user=False)
                os.makedirs('/scratch', exist_ok=True)
                run(['mount', '-o', 'nodev,nosuid', '/dev/vdb', '/scratch'], {'PATH': SYSTEM_PATH}, as_user=False)
                chown('/scratch')
                work = '/scratch/work'
        os.makedirs(STATE, mode=0o711, exist_ok=True)
        os.chmod(STATE, 0o711)
        with open(HELPER, 'w') as f:
            f.write(HELPER_SCRIPT)
        os.chmod(HELPER, 0o755)

        env = {'HOME': HOME, 'USER': USER or 'runner', 'LOGNAME': USER or 'runner', 'SHELL': '/bin/bash',
               'LANG': 'C.UTF-8', 'TERM': 'dumb', 'PATH': os.path.join(HOME, '.local/bin') + ':' + SYSTEM_PATH}
        if ACCOUNT is None:
            env['PATH'] = os.environ.get('PATH', SYSTEM_PATH)
        env.update(spec.get('env') or {})
        install_env = {'HOME': '/root' if ACCOUNT is not None else HOME, 'PATH': env['PATH'], 'LANG': 'C.UTF-8'}
        private_dir(CONTROL)
        place_files(spec.get('files') or [])
        prompt_file = os.path.join(CONTROL, 'prompt.md')
        with open(prompt_file, 'w', encoding='utf-8') as f:
            f.write(spec['prompt'])
        os.chmod(prompt_file, 0o600)
        chown(prompt_file)

        agent = spec['agent']
        if agent.get('runtime') == 'node':
            ensure_node(install_env, clock)
        binary = agent.get('binary')
        if agent.get('install') and not (binary and shutil.which(binary, path=env['PATH'])):
            status('installing agent %s' % agent['name'])
            retry('agent install', lambda: run(['bash', '-c', agent['install']], install_env, cwd='/', as_user=False,
                                               timeout=clock.budget(1800, clock.agent_end)))

        repository = spec.get('repository')
        branch = spec.get('branch')
        token = spec.get('gitToken', '')
        git_host = (urllib.parse.urlparse(repository).hostname or '').lower() if repository else ''
        git_auth = ['-c', 'credential.helper=', '-c', 'credential.helper=' + HELPER]
        user_git = dict(env, GIT_TERMINAL_PROMPT='0')
        default_branch = ''
        if repository:
            status('cloning repository')
            clone_env = dict(user_git, RUNNERLOOM_GIT_TOKEN=token, RUNNERLOOM_GIT_HOST=git_host)

            def clone():
                shutil.rmtree(work, ignore_errors=True)
                run(['git'] + git_auth + ['clone', '--no-tags', '--', repository, work], clone_env,
                    cwd=HOME, timeout=clock.budget(1800, clock.agent_end))
            retry('clone', clone)
            _, head = run(['git', 'symbolic-ref', '--short', 'refs/remotes/origin/HEAD'], user_git, cwd=work, check=False, timeout=30)
            default_branch = head.strip().removeprefix('origin/')
            if branch in (default_branch, spec.get('baseRef')):
                raise StepError('refusing to work on the base or default branch %s' % branch)
            remote = run(['git', 'rev-parse', '--verify', '--quiet', 'refs/remotes/origin/' + branch], user_git, cwd=work, check=False, timeout=30)[0] == 0
            if remote:
                status('continuing existing branch')
                run(['git', 'checkout', '-q', '-B', branch, 'refs/remotes/origin/' + branch], user_git, cwd=work)
            elif spec.get('baseRef'):
                run(['git', 'checkout', '-q', '-B', branch, 'refs/remotes/origin/' + spec['baseRef']], user_git, cwd=work)
            else:
                run(['git', 'checkout', '-q', '-B', branch], user_git, cwd=work)
        else:
            private_dir(work)
            run(['git', 'init', '-q', '-b', 'runnerloom-task'], user_git, cwd=work)
        run(['git', 'config', 'user.name', 'RunnerLoom Agent'], user_git, cwd=work)
        run(['git', 'config', 'user.email', 'runnerloom-agent@users.noreply.invalid'], user_git, cwd=work)
        if not repository:
            run(['git', 'commit', '-q', '--allow-empty', '-m', 'RunnerLoom task workspace'], user_git, cwd=work)
        base_commit = run(['git', 'rev-parse', 'HEAD'], user_git, cwd=work)[1].strip()

        argv = [a.replace('{promptFile}', prompt_file).replace('{prompt}', spec['prompt']) for a in agent['run']]
        resolved = shutil.which(argv[0], path=env['PATH'])
        if not resolved:
            raise StepError('agent command not found: %s' % argv[0])
        argv[0] = resolved
        status('running agent')
        code, output, truncated, timed_out = run_agent(argv, env, work, clock, mirror)
        result.update(exitCode=code, output=output, truncated=truncated,
                      status='succeeded' if code == 0 else 'failed')
        if timed_out:
            result['error'] = 'agent exceeded the task time limit'
        status('agent exited with %d' % code)

        # Commit as the agent user WITHOUT any secret: the work tree and its
        # .git/config belong to the agent and may run its programs.
        safe = ['-c', 'core.hooksPath=/dev/null', '-c', 'core.fsmonitor=false', '-c', 'commit.gpgsign=false']
        run(['git'] + safe + ['add', '-A'], user_git, cwd=work, timeout=clock.budget(300))
        if run(['git'] + safe + ['status', '--porcelain'], user_git, cwd=work, timeout=clock.budget(120))[1].strip():
            message = 'RunnerLoom task %s: %s\n\nAgent: %s' % (task_id[:8], title[:120], agent['name'])
            run(['git'] + safe + ['commit', '-q', '-m', message], user_git, cwd=work, timeout=clock.budget(120))
        kill_user_processes()

        # Root copies the commits into a fresh repository with no agent-owned
        # configuration, then pushes from there with the token.
        shutil.rmtree(CLEAN, ignore_errors=True)
        private_dir(CLEAN, owner=False)
        root_git = {'PATH': SYSTEM_PATH if ACCOUNT is not None else env['PATH'], 'HOME': STATE, 'LANG': 'C.UTF-8',
                    'GIT_CONFIG_NOSYSTEM': '1', 'GIT_CONFIG_GLOBAL': os.devnull, 'GIT_TERMINAL_PROMPT': '0'}

        def clean_git(args, extra=None, cap=300, check=True):
            return run(['git', '-C', CLEAN] + args, dict(root_git, **(extra or {})), as_user=False, timeout=clock.budget(cap), check=check)
        clean_git(['init', '-q'])
        # The local transport drops -c settings for the spawned upload-pack,
        # so the ownership exception must be given to upload-pack itself.
        clean_git(['-c', 'protocol.file.allow=always', 'fetch', '-q', '--no-tags',
                   '--upload-pack=git -c safe.directory=* upload-pack', 'file://' + work, '+HEAD:refs/runnerloom/head'], cap=600)
        head = clean_git(['rev-parse', 'refs/runnerloom/head'])[1].strip()
        result['commit'] = head
        result['changed'] = head != base_commit
        if repository:
            result['branch'] = branch
        has_base = clean_git(['cat-file', '-e', base_commit + '^{commit}'], check=False)[0] == 0
        if result['changed'] and has_base:
            result['diffStat'] = tail(clean_git(['diff', '--stat', base_commit, head])[1], DIFFSTAT_LIMIT)[0]
        if result['changed'] and repository and token:
            status('pushing branch')
            push_env = {'RUNNERLOOM_GIT_TOKEN': token, 'RUNNERLOOM_GIT_HOST': git_host}

            def push():
                clean_git(git_auth + ['push', repository, 'refs/runnerloom/head:refs/heads/' + branch], push_env, cap=900)
            try:
                retry('push', push)
                result['pushed'] = True
            except StepError as error:
                result['error'] = ('push failed: ' + str(error)[:1500]).strip()
        if result['pushed'] and spec.get('pullRequest'):
            try:
                result['pullRequestURL'] = pull_request(spec, task_id, title, default_branch, clock)
            except Exception as error:  # the branch is already safe on GitHub
                result['error'] = 'pull request failed: %s' % str(error)[:500]
        if result['changed'] and not result['pushed']:
            if not has_base:
                result['error'] = (result.get('error', '') + ' the agent rewrote history; no patch against the base').strip()
            else:
                patch = clean_git(['diff', '--binary', base_commit, head])[1]
                if len(patch.encode('utf-8', 'replace')) > PATCH_LIMIT:
                    result['truncated'] = True
                    result['error'] = (result.get('error', '') + ' patch exceeded the return limit').strip()
                else:
                    result['patch'] = patch
    except BaseException as error:
        result['status'] = 'failed'
        result['error'] = ('%s: %s' % (type(error).__name__, error))[:4000]
    finally:
        try:
            try:
                kill_user_processes()
            except Exception:
                pass
            emit(result, mirror)
        finally:
            sys.stdout.flush()
            try:
                # Let the serial port transmit every copy before shutdown.
                termios.tcdrain(sys.stdout.fileno())
            except (OSError, ValueError, termios.error):
                pass
            if POWEROFF:
                subprocess.run(['sync'], check=False)
                subprocess.run(['systemctl', 'poweroff', '--no-block'], check=False)


if __name__ == '__main__':
    main()
