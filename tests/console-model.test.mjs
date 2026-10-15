import { test } from 'node:test';
import assert from 'node:assert/strict';
import { snapshot, overlaps, intersects, backoff, sameIdentity, projectName, verdictOf, sessionFrom, accessOffered, aiRecord, aiAskRoute, aiCounts, deciderName, isAI, aiModel, cloneCommand, filterProjects, projectSummary } from '../web/console/model.js';
const intent = (id, extra = {}) => ({ id, agent: id, status: 'open', created_at: id, declared_paths: ['src/**'], actual_paths: [], ...extra });
test('only the latest active human decision appears in review', () => {
  const intents = [intent('a'), intent('b', {status: 'landed'}), intent('c'), intent('d'), intent('e')];
  const decision = (id, intent_id, action, status) => ({id, intent_id, action, status, created_at: id});
  const decisions = [decision('1', 'a', 'human_approve', 'pending_human'), decision('2', 'a', 'block', 'blocked'), decision('3', 'b', 'human_approve', 'pending_human'), decision('4', 'c', 'quarantine', 'quarantined'), decision('5', 'd', 'agent_review', 'pending_agent'), decision('6', 'e', 'human_approve', 'superseded')];
  assert.deepEqual(snapshot(intents, decisions).waiting.map(x => x.intent.id), ['c']);
});
test('overlap suggestions follow proposed, pushed and declared work', () => {
  const scenarios = [
    [{}, {}, 'split', 'Split'],
    [{actual_paths: ['src/a']}, {actual_paths: ['src/a']}, 'stack', 'Stack b on a'],
    [{}, {status: 'proposed'}, 'land_first', 'Land b before a'],
    [{}, {actual_paths: ['src/a']}, 'land_first', 'Land b before a'],
    [{declared_paths: ['src/a']}, {declared_paths: ['src/a']}, 'land_first', 'Land a before b'],
  ];
  for (const [a, b, action, text] of scenarios) {
    const [o] = overlaps([intent('a', a), intent('b', b)]);
    assert.equal(o.action, action); assert.ok(o.suggestion.startsWith(text));
  }
  assert.equal(overlaps([intent('a'), intent('b', {parent_id: 'a'})]).length, 0);
  assert.equal(overlaps([intent('a'), intent('b', {status: 'landed'})]).length, 0);
});
test('path intersections cover literal paths, wildcard directories and conservative glob pairs', () => {
  for (const [a, b, want] of [['**/*.md','README.md',true], ['docs/*','docs/nested/a',false], ['src/**','docs/**',false], ['src/?','src/a',true], ['a.ts','b.ts',false], ['src/**','**/*.ts',true]]) assert.equal(intersects(a,b), want);
});
test('identity, project validation and bounded retry delay', () => {
  assert.ok(sameIdentity(' Alice ', 'alice'));
  assert.ok(projectName('demo-test-console')); assert.ok(!projectName('https://elsewhere'));
  assert.deepEqual([0,1,2,3,4,100].map(backoff), [2000,4000,8000,16000,30000,30000]);
});
test('only a pressed Approve or Reject button yields a verdict', () => {
  assert.equal(verdictOf('approve'), 'approve'); assert.equal(verdictOf('reject'), 'reject');
  for (const v of [undefined, null, '', 'land', 'APPROVE', 0]) assert.equal(verdictOf(v), null);
});
test('a stored session needs a key and is dropped once past its expiry', () => {
  const now = Date.parse('2026-10-14T12:00:00Z');
  const live = { key: 'k', name: 'a@x', source: 'access', expires_at: '2026-10-14T23:00:00Z' };
  assert.deepEqual(sessionFrom(JSON.stringify(live), now), live);
  assert.deepEqual(sessionFrom(JSON.stringify({ key: 'k', name: 'n' }), now), { key: 'k', name: 'n' });
  for (const raw of [null, '', 'null', '{', '[]', '"k"', JSON.stringify({ key: '' }), JSON.stringify({ key: 1 }), JSON.stringify({ name: 'n' }),
    JSON.stringify({ ...live, expires_at: '2026-10-14T11:59:59Z' }), JSON.stringify({ ...live, expires_at: 'soon' })])
    assert.equal(sessionFrom(raw, now), null, raw);
});
test('Access sign-in is offered only on a redirect or a method refusal, never on a failure', () => {
  assert.ok(accessOffered({ type: 'opaqueredirect' }));
  assert.ok(accessOffered({ type: 'basic', code: 'method_not_allowed' }));
  for (const r of [{ type: 'basic', code: 'access_not_configured' }, { type: 'basic', code: 'not_found' }, { type: 'basic' }, {}, undefined])
    assert.equal(accessOffered(r), false, JSON.stringify(r));
});
test('the AI reviewer is recognised by identity source or by its name, and named by its model', () => {
  const m = '@cf/meta/llama-4-scout-17b-16e-instruct';
  assert.ok(isAI({ by: 'workers-ai:' + m }));
  assert.ok(isAI({ by: 'anything', identity_source: 'ai' }));
  assert.ok(!isAI({ by: 'alice', identity_source: 'access' }));
  assert.equal(aiModel('workers-ai:' + m), m); assert.equal(aiModel('alice'), ''); assert.equal(aiModel(undefined), '');
  assert.equal(deciderName({ by: 'workers-ai:' + m, identity_source: 'ai' }), `AI reviewer (${m})`);
  assert.equal(deciderName({ by: 'alice', identity_source: 'access' }), 'alice (Access)');
  assert.equal(deciderName({ by: 'bob' }), 'bob');
  assert.equal(deciderName({ by: 'admin', claimed_by: 'john' }), 'admin (john)');
});
test('the AI record on a decision is read defensively', () => {
  const m = 'm1';
  const approved = { decided_by: 'workers-ai:' + m, approvals: [{ by: 'workers-ai:' + m, verdict: 'approve', reason: 'fine', identity_source: 'ai' }] };
  assert.deepEqual(aiRecord(approved), { decided: true, model: m, approval: approved.approvals[0], escalation: null, advice: null });
  const escalated = { status: 'pending_human', detail: { escalation: { by: 'workers-ai:' + m, reason: 'new host', concerns: ['a', ' b ', '', 3], from_action: 'agent_review', ai: { model: 'm2' } } } };
  const r = aiRecord(escalated);
  assert.equal(r.decided, true); assert.equal(r.model, 'm2');
  assert.deepEqual(r.escalation, { reason: 'new host', concerns: ['a', 'b'], from: 'agent_review', model: 'm2' });
  const advised = { status: 'pending_human', detail: { ai_advice: { summary: 's', risks: ['r'], questions: ['q'], by: 'workers-ai:' + m } } };
  const a = aiRecord(advised);
  assert.equal(a.decided, false); assert.equal(a.model, m);
  assert.deepEqual(a.advice, { summary: 's', risks: ['r'], concerns: [], questions: ['q'], model: m });
  for (const d of [{}, { detail: {} }, { detail: { escalation: 'x', ai_advice: 7 } }, { decided_by: 'alice', approvals: [{ by: 'alice' }] }, null, undefined])
    assert.deepEqual(aiRecord(d), { decided: false, model: '', approval: null, escalation: null, advice: null }, JSON.stringify(d));
  assert.deepEqual(aiCounts([approved, escalated, advised, {}]), { approved: 1, escalated: 1, advised: 1 });
});
test('Ask AI asks for a verdict only while a reviewer agent is awaited, else advice', () => {
  assert.equal(aiAskRoute({ status: 'pending_agent' }), 'ai-review');
  assert.equal(aiAskRoute({ status: 'pending_human' }), 'ai-advice');
  assert.equal(aiAskRoute({ status: 'quarantined' }), 'ai-advice');
  for (const s of ['approved', 'rejected', 'blocked', 'superseded', undefined]) assert.equal(aiAskRoute({ status: s }), null);
  assert.equal(aiAskRoute(null), null);
});

test('the project directory filters as typed, case-insensitively, without reordering', () => {
  const rows = [{ name: 'demo-1791938487' }, { name: 'eu-demo-1791797572' }, { name: 'wayglass' }];
  assert.deepEqual(filterProjects(rows, '').map((p) => p.name), ['demo-1791938487', 'eu-demo-1791797572', 'wayglass']);
  assert.deepEqual(filterProjects(rows, ' DEMO ').map((p) => p.name), ['demo-1791938487', 'eu-demo-1791797572']);
  assert.deepEqual(filterProjects(rows, 'eu-'), [rows[1]]);
  assert.deepEqual(filterProjects(rows, 'nothing'), []);
  assert.notEqual(filterProjects(rows, ''), rows, 'the caller keeps its array');
});
test('a project row summarises its counts, the public shape too, and says when it is not served', () => {
  assert.deepEqual(projectSummary({ name: 'a', counts: { intents: 3, open: 2, agents: 2, pending_human: 1, landed: 4 } }),
    { served: true, text: '2 open · 1 for a person · 4 landed · 2 agents', needsYou: 1 });
  assert.deepEqual(projectSummary({ name: 'a', counts: { intents: 1, open: 1, agents: 1 } }),
    { served: true, text: '1 open · 1 agent', needsYou: 0 });
  assert.deepEqual(projectSummary({ name: 'a', counts: null, residency: 'residency_pending' }),
    { served: false, text: 'not served: residency_pending', needsYou: 0 });
  assert.equal(cloneCommand('eu-demo-1791797572'), 'wayglass clone eu-demo-1791797572');
});

 test("third-party AI verdicts retain their model names", () => {
  for (const [by, model] of [["anthropic:claude-sonnet-5.5", "claude-sonnet-5.5"], ["openai:gpt-5.4-mini", "gpt-5.4-mini"]]) {
    assert.equal(aiModel(by), model);
    assert.equal(deciderName({by, identity_source: "ai"}), `AI reviewer (${model})`);
  }
});
