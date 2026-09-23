#!/usr/bin/python3
"""RunnerLoom agent-task runner.

Runs as root inside a disposable guest VM. The coding agent itself runs as the
unprivileged runner user. Everything printed here goes to the serial console,
which the Node keeps as a bounded owner-only log. Agent output is mirrored with
a "| " prefix so it can never look like a result marker; the final result is a
chunked, SHA-256 checked JSON document that the Node reports to the Controller.

The RUNNERLOOM_TASK_* environment variables exist only so the repository tests
can exercise this file without a VM. cloud-init never sets them.
"""
import base64
import collections
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
import threading
import time
import urllib.error
import urllib.parse
import urllib.request

PAYLOAD = os.environ.get('RUNNERLOOM_TASK_PAYLOAD', '/run/runnerloom-task.json')
USER = os.environ.get('RUNNERLOOM_TASK_USER', 'runner')
HOME = os.environ.get('RUNNERLOOM_TASK_HOME', '/home/runner')
POWEROFF = os.environ.get('RUNNERLOOM_TASK_POWEROFF', '1') == '1'
WORK = os.path.join(HOME, 'work')
CONTROL = os.path.join(HOME, '.runnerloom')
OUTPUT_LIMIT = 48 << 10
PATCH_LIMIT = 384 << 10
DIFFSTAT_LIMIT = 16 << 10
MIRROR_LIMIT = 8 << 20
MIRROR_LINE = 2000
CHUNK = 1024
GIT_HELPER = '!f() { test "$1" = get && printf "username=x-access-token\\npassword=%s\\n" "$RUNNERLOOM_GIT_TOKEN"; }; f'


class StepError(Exception):
    pass


def status(message):
    print('RUNNERLOOM_TASK ' + message, flush=True)


def tail(text, limit):
    data = text.encode('utf-8', 'replace')
    if len(data) <= limit:
        return text, False
    return data[-limit:].decode('utf-8', 'ignore'), True


def account():
    if not USER:
        return None
    entry = pwd.getpwnam(USER)
    return entry.pw_uid, entry.pw_gid


ACCOUNT = None


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


def private_dir(path):
    os.makedirs(path, mode=0o700, exist_ok=True)
    os.chmod(path, 0o700)
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


def fetch(url, timeout=120):
    with urllib.request.urlopen(urllib.request.Request(url, headers={'User-Agent': 'runnerloom-task'}), timeout=timeout) as response:
        return response.read()


def ensure_node(env):
    node = shutil.which('node', path=env['PATH'])
    if node:
        _, version = run([node, '--version'], env, as_user=False, check=False)
        match = re.match(r'v(\d+)\.', version.strip())
        if match and int(match.group(1)) >= 20:
            return
    status('installing Node.js 22 (SHA-256 checked against nodejs.org SHASUMS256.txt)')
    base = 'https://nodejs.org/dist/latest-v22.x/'
    sums = fetch(base + 'SHASUMS256.txt').decode()
    match = re.search(r'^([a-f0-9]{64})\s+(node-v22\.\d+\.\d+-linux-x64\.tar\.xz)$', sums, re.M)
    if not match:
        raise StepError('Node.js release index had no linux-x64 archive')
    archive = fetch(base + match.group(2), timeout=600)
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


def git(args, env, check=True, timeout=900):
    argv = ['git', '-c', 'credential.helper=', '-c', 'credential.helper=' + GIT_HELPER, '-c', 'core.hooksPath=/dev/null'] + args
    return run(argv, env, cwd=WORK if os.path.isdir(WORK) else HOME, timeout=timeout, check=check)


def github_api(method, path, token, body=None):
    data = json.dumps(body).encode() if body is not None else None
    request = urllib.request.Request('https://api.github.com' + path, data=data, method=method, headers={
        'Authorization': 'Bearer ' + token, 'Accept': 'application/vnd.github+json',
        'X-GitHub-Api-Version': '2022-11-28', 'User-Agent': 'runnerloom-task', 'Content-Type': 'application/json'})
    with urllib.request.urlopen(request, timeout=60) as response:
        return json.loads(response.read().decode() or 'null')


def pull_request(spec, task_id, title):
    token = spec['gitToken']
    parsed = urllib.parse.urlparse(spec['repository'])
    owner, repo = parsed.path.strip('/').removesuffix('.git').split('/')
    quoted = '/repos/%s/%s' % (urllib.parse.quote(owner), urllib.parse.quote(repo))
    base = spec.get('baseRef') or github_api('GET', quoted, token)['default_branch']
    body = 'RunnerLoom task `%s` (agent: `%s`).\n\n### Prompt\n\n%s' % (task_id, spec['agent']['name'], spec['prompt'][:60000])
    try:
        return github_api('POST', quoted + '/pulls', token, {'title': title, 'head': spec['branch'], 'base': base, 'body': body, 'draft': True})['html_url']
    except urllib.error.HTTPError as error:
        if error.code != 422:
            raise
    query = urllib.parse.urlencode({'head': owner + ':' + spec['branch'], 'state': 'open'})
    found = github_api('GET', quoted + '/pulls?' + query, token)
    if found:
        return found[0]['html_url']
    raise StepError('pull request could not be created or found')


def run_agent(argv, env, deadline):
    process = subprocess.Popen(argv, env=env, cwd=WORK, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                               stderr=subprocess.STDOUT, start_new_session=True, **demote())
    buffer = collections.deque()
    state = {'size': 0, 'mirrored': 0}

    def read():
        for raw in iter(process.stdout.readline, b''):
            line = raw.decode('utf-8', 'replace').rstrip('\r\n')
            if state['mirrored'] < MIRROR_LIMIT:
                shown = line[:MIRROR_LINE]
                print('| ' + shown, flush=True)
                state['mirrored'] += len(shown) + 3
            buffer.append(line)
            state['size'] += len(line) + 1
            while state['size'] > OUTPUT_LIMIT * 2 and buffer:
                state['size'] -= len(buffer.popleft()) + 1

    reader = threading.Thread(target=read, daemon=True)
    reader.start()
    timed_out = False
    try:
        process.wait(timeout=max(1, deadline - time.monotonic()))
    except subprocess.TimeoutExpired:
        timed_out = True
        os.killpg(process.pid, signal.SIGTERM)
        try:
            process.wait(timeout=30)
        except subprocess.TimeoutExpired:
            os.killpg(process.pid, signal.SIGKILL)
            process.wait()
    reader.join(timeout=10)
    output, truncated = tail('\n'.join(buffer), OUTPUT_LIMIT)
    return (124 if timed_out else process.returncode), output, truncated, timed_out


def emit(result):
    data = json.dumps(result, ensure_ascii=False, separators=(',', ':')).encode('utf-8')
    encoded = base64.b64encode(data).decode()
    chunks = [encoded[i:i + CHUNK] for i in range(0, len(encoded), CHUNK)] or ['']
    for index, chunk in enumerate(chunks):
        print('RUNNERLOOM_TASK_RESULT %d %s' % (index, chunk), flush=True)
    print('RUNNERLOOM_TASK_RESULT_END %d %s' % (len(chunks), hashlib.sha256(data).hexdigest()), flush=True)


def main():
    global ACCOUNT
    started = time.monotonic()
    result = {'status': 'failed', 'exitCode': -1, 'changed': False, 'pushed': False}
    try:
        with open(PAYLOAD, encoding='utf-8') as f:
            document = json.load(f)
        try:
            os.remove(PAYLOAD)
        except OSError:
            pass
        spec = document['spec']
        task_id = document['id']
        timeout = max(60, int(document.get('timeoutSeconds', 3600)))
        # Leave time for commit, push, pull request and result output.
        deadline = started + max(30, timeout - 240)
        ACCOUNT = account()
        status('started task %s agent=%s' % (task_id, spec['agent']['name']))
        if ACCOUNT is not None:
            subprocess.run(['dmesg', '-n', '1'], check=False)
            if os.path.exists('/dev/vdb'):
                run(['mkfs.ext4', '-q', '/dev/vdb'], os.environ.copy(), as_user=False)
                os.makedirs('/scratch', exist_ok=True)
                run(['mount', '-o', 'nodev,nosuid', '/dev/vdb', '/scratch'], os.environ.copy(), as_user=False)
                chown('/scratch')
        system_path = '/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'
        env = {'HOME': HOME, 'USER': USER or 'runner', 'LOGNAME': USER or 'runner', 'SHELL': '/bin/bash',
               'LANG': 'C.UTF-8', 'TERM': 'dumb', 'PATH': os.path.join(HOME, '.local/bin') + ':' + system_path}
        if ACCOUNT is None:
            env['PATH'] = os.environ.get('PATH', system_path)
        env.update(spec.get('env') or {})
        private_dir(CONTROL)
        place_files(spec.get('files') or [])
        prompt_file = os.path.join(CONTROL, 'prompt.md')
        with open(prompt_file, 'w', encoding='utf-8') as f:
            f.write(spec['prompt'])
        os.chmod(prompt_file, 0o600)
        chown(prompt_file)

        agent = spec['agent']
        root_env = dict(env, HOME='/root') if ACCOUNT is not None else env
        if agent.get('runtime') == 'node':
            ensure_node(root_env)
        binary = agent.get('binary')
        if agent.get('install') and not (binary and shutil.which(binary, path=env['PATH'])):
            status('installing agent %s' % agent['name'])
            run(['bash', '-c', agent['install']], root_env, cwd='/', as_user=False, timeout=1800)

        git_env = dict(env, GIT_TERMINAL_PROMPT='0', RUNNERLOOM_GIT_TOKEN=spec.get('gitToken', ''))
        repository = spec.get('repository')
        branch = spec.get('branch')
        if repository:
            status('cloning repository')
            run(['git', '-c', 'credential.helper=', '-c', 'credential.helper=' + GIT_HELPER, 'clone', '--no-tags', '--', repository, WORK],
                git_env, cwd=HOME, timeout=1800)
            remote = git(['ls-remote', '--exit-code', '--heads', 'origin', branch], git_env, check=False)[0] == 0
            if remote:
                status('continuing existing branch')
                git(['checkout', '-B', branch, 'refs/remotes/origin/' + branch], git_env)
            elif spec.get('baseRef'):
                git(['checkout', '-B', branch, 'refs/remotes/origin/' + spec['baseRef']], git_env)
            else:
                git(['checkout', '-B', branch], git_env)
        else:
            private_dir(WORK)
            git(['init', '-q', '-b', 'runnerloom-task'], git_env)
        git(['config', 'user.name', 'RunnerLoom Agent'], git_env)
        git(['config', 'user.email', 'runnerloom-agent@users.noreply.invalid'], git_env)
        if not repository:
            git(['commit', '-q', '--allow-empty', '-m', 'RunnerLoom task workspace'], git_env)
        base_commit = git(['rev-parse', 'HEAD'], git_env)[1].strip()

        argv = [a.replace('{promptFile}', prompt_file).replace('{prompt}', spec['prompt']) for a in agent['run']]
        resolved = shutil.which(argv[0], path=env['PATH'])
        if not resolved:
            raise StepError('agent command not found: %s' % argv[0])
        argv[0] = resolved
        status('running agent')
        code, output, truncated, timed_out = run_agent(argv, env, deadline)
        result.update(exitCode=code, output=output, truncated=truncated,
                      status='succeeded' if code == 0 else 'failed')
        if timed_out:
            result['error'] = 'agent exceeded the task time limit'
        status('agent exited with %d' % code)

        git(['add', '-A'], git_env)
        if git(['status', '--porcelain'], git_env)[1].strip():
            title = document.get('title') or 'RunnerLoom task'
            message = 'RunnerLoom task %s: %s\n\nAgent: %s' % (task_id[:8], title[:120], agent['name'])
            git(['commit', '-q', '-m', message], git_env)
        head = git(['rev-parse', 'HEAD'], git_env)[1].strip()
        result['commit'] = head
        result['changed'] = head != base_commit
        if repository:
            result['branch'] = branch
        if result['changed']:
            result['diffStat'] = tail(git(['diff', '--stat', base_commit, head], git_env)[1], DIFFSTAT_LIMIT)[0]
        if result['changed'] and repository and spec.get('gitToken'):
            status('pushing branch')
            code, pushed = git(['push', 'origin', 'HEAD:refs/heads/' + branch], git_env, check=False)
            result['pushed'] = code == 0
            if code != 0:
                result['error'] = ('push failed: ' + tail(pushed, 1500)[0]).strip()
        if result['pushed'] and spec.get('pullRequest'):
            try:
                result['pullRequestURL'] = pull_request(spec, task_id, document.get('title') or 'RunnerLoom task')
            except Exception as error:  # the branch is already safe on GitHub
                result['error'] = 'pull request failed: %s' % str(error)[:500]
        if result['changed'] and not result['pushed']:
            patch = git(['diff', '--binary', base_commit, head], git_env)[1]
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
            emit(result)
        finally:
            sys.stdout.flush()
            if POWEROFF:
                subprocess.run(['sync'], check=False)
                subprocess.run(['systemctl', 'poweroff', '--no-block'], check=False)


if __name__ == '__main__':
    main()
