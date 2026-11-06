# awflex callback plugin: one JSON line per host result, appended to the file
# named by AWFLEX_EVENTS. The server reads it to give every host in a shared
# ansible run its own status, failed task and log.
from __future__ import annotations

import json
import os

from ansible.plugins.callback import CallbackBase

DOCUMENTATION = '''
    name: awflex
    type: aggregate
    short_description: per-host JSON lines for awflex
    description:
      - Appends one JSON object per host task result and per host summary to the file named by AWFLEX_EVENTS.
'''

LIMIT = 4000


def _text(value):
    if value is None:
        return ''
    if not isinstance(value, str):
        try:
            value = json.dumps(value, default=str)
        except Exception:
            value = str(value)
    return value if len(value) <= LIMIT else value[:LIMIT] + ' ...[truncated]'


class CallbackModule(CallbackBase):
    CALLBACK_VERSION = 2.0
    CALLBACK_TYPE = 'aggregate'
    CALLBACK_NAME = 'awflex'
    CALLBACK_NEEDS_ENABLED = True
    CALLBACK_NEEDS_WHITELIST = True

    def __init__(self):
        super().__init__()
        path = os.environ.get('AWFLEX_EVENTS')
        self._fh = open(path, 'a', buffering=1) if path else None

    def _emit(self, **event):
        if self._fh:
            self._fh.write(json.dumps(event, default=str) + '\n')

    @staticmethod
    def _parts(result):
        # ansible-core 2.19 renamed the private attributes; support both.
        try:
            return result.host, result.task, result.result
        except AttributeError:
            return result._host, result._task, result._result

    def _on(self, result, status, ignore_errors=False):
        host, task, res = self._parts(result)
        event = {'e': 'task', 'host': host.get_name(), 'task': task.get_name(), 'status': status}
        if status == 'ok' and res.get('changed'):
            event['status'] = 'changed'
        if ignore_errors:
            event['ignore_errors'] = True
        if status in ('failed', 'unreachable'):
            if res.get('_ansible_no_log') or 'censored' in res:
                event['msg'] = 'output hidden (no_log)'
            else:
                msgs = [_text(res.get('msg'))]
                for item in res.get('results') or []:
                    if isinstance(item, dict) and item.get('failed') and item.get('msg'):
                        msgs.append(_text(item.get('msg')))
                event['msg'] = '; '.join(m for m in msgs if m)
                if res.get('stderr'):
                    event['stderr'] = _text(res.get('stderr'))
                if res.get('stdout'):
                    event['stdout'] = _text(res.get('stdout'))
        self._emit(**event)

    def v2_runner_on_ok(self, result):
        self._on(result, 'ok')

    def v2_runner_on_failed(self, result, ignore_errors=False):
        self._on(result, 'failed', ignore_errors)

    def v2_runner_on_unreachable(self, result):
        self._on(result, 'unreachable')

    def v2_runner_on_skipped(self, result):
        self._on(result, 'skipped')

    def v2_playbook_on_stats(self, stats):
        for host in sorted(stats.processed.keys()):
            self._emit(e='stats', host=host, **stats.summarize(host))
