"""Tests for the Discord Q&A hook in the embedded bridge.

When a director-installed router exists at ``<CONDUCTOR_DIR>/director/discord_qna.py``,
Discord question buttons (``qna:<id>:<choice>`` custom ids) and replies are
routed to it. agent-deck does not ship that router. With no router file a reply
does nothing and a button fails closed. The contract is section 5.3 of
docs/plans/2026-10-31-conductor-setup-safe-reinstall-design.md.

No discord.py is needed and nothing is skipped: the ``qdir`` fixture swaps a
fake ``discord`` into the bridge, and every router here is a throwaway script
in a temporary conductor directory. The tests read each new name as
``bridge.<name>`` at call time, so this file collects on any version of the bridge.
"""

from __future__ import annotations

import asyncio
import inspect
import json
import subprocess
from types import SimpleNamespace
from unittest import mock

import pytest

import bridge  # conftest.py registers the canonical bridge under this name

COMPONENT, NO_MENTIONS = object(), object()
QID, CHANNEL, USER = "0123456789ab", 222, 333
ANSWER = "Answered Q:0123456789ab."
HANDLED = 'print(json.dumps({"handled": True, "response": %r}))' % ANSWER
UNHANDLED = 'print(json.dumps({"handled": False}))'
NO_RESPONSE = 'print(json.dumps({"handled": True}))'
NULL_RESPONSE = 'print(json.dumps({"handled": True, "response": None}))'
RECORDER = 'pathlib.Path(sys.argv[0]).with_name("event.json").write_text(sys.stdin.read()); ' + UNHANDLED
UNCONFIRMED = "Answer outcome unconfirmed. Check the session locally; do not retry automatically."
UNAVAILABLE = "Reply routing unavailable; nothing forwarded. Please check the session locally."


@pytest.fixture
def qdir(tmp_path, monkeypatch):
    """A temporary conductor directory with an empty ``director/`` and a fake discord."""
    monkeypatch.setattr(
        bridge,
        "discord",
        SimpleNamespace(
            InteractionType=SimpleNamespace(component=COMPONENT),
            AllowedMentions=SimpleNamespace(none=lambda: NO_MENTIONS),
        ),
        raising=False,
    )
    monkeypatch.setattr(bridge, "CONDUCTOR_DIR", tmp_path)
    (tmp_path / "director").mkdir()
    return tmp_path


def _router(qdir, body):
    (qdir / "director" / "discord_qna.py").write_text("import json, pathlib, sys, time\n" + body + "\n")


def _authorized(user_id):
    return user_id == USER


def _interaction(custom_id=f"qna:{QID}:2", type_=COMPONENT, channel_id=CHANNEL, user_id=USER, message_id=444):
    return SimpleNamespace(
        type=type_,
        data={"custom_id": custom_id},
        channel_id=channel_id,
        id=555,
        user=SimpleNamespace(id=user_id),
        message=None if message_id is None else SimpleNamespace(id=message_id),
        response=SimpleNamespace(send_message=mock.AsyncMock(), defer=mock.AsyncMock()),
        edit_original_response=mock.AsyncMock(),
    )


def _message(reference_id=666, channel_id=CHANNEL, author_id=USER, content="<@999> 2"):
    return SimpleNamespace(
        id=777,
        content=content,
        author=SimpleNamespace(id=author_id, bot=False),
        channel=SimpleNamespace(id=channel_id),
        reference=None if reference_id is None else SimpleNamespace(message_id=reference_id),
        reply=mock.AsyncMock(),
    )


def _button(i):
    return asyncio.run(bridge.qna_handle_interaction(i, CHANNEL, _authorized))


def _reply(m):
    return asyncio.run(bridge.qna_handle_reply(m, CHANNEL, _authorized))


def test_route_question_returns_router_result(qdir):
    _router(qdir, HANDLED)
    assert bridge.qna_router_path() == qdir / "director" / "discord_qna.py"
    assert bridge.route_question({"reference": "1"}) == {"handled": True, "response": ANSWER}
    _router(qdir, UNHANDLED)
    assert bridge.route_question({"reference": "1"}) == {"handled": False}


def test_route_question_nonzero_exit_raises(qdir):
    _router(qdir, "sys.exit(3)")
    with pytest.raises(RuntimeError, match="Question router failed"):
        bridge.route_question({"reference": "1"})


@pytest.mark.parametrize(
    "body",
    [
        'print("not json")',
        'print(json.dumps(["handled"]))',
        'print(json.dumps({"response": "x"}))',
        'print(json.dumps({"handled": "yes"}))',
    ],
    ids=["bad-json", "not-a-dict", "no-handled", "handled-not-a-bool"],
)
def test_route_question_rejects_invalid_result(qdir, body):
    _router(qdir, body)
    with pytest.raises(ValueError):
        bridge.route_question({"reference": "1"})


def test_route_question_timeout(qdir):
    _router(qdir, "time.sleep(60)")
    assert bridge.QNA_ROUTER_TIMEOUT == 90
    assert inspect.signature(bridge.route_question).parameters["timeout"].default == 90
    with pytest.raises(subprocess.TimeoutExpired):
        bridge.route_question({"reference": "1"}, timeout=1)


@pytest.mark.parametrize(
    "custom_id, choice",
    [
        (f"qna:{QID}:7", "7"),
        (f"qna:{QID}:12", "12"),
        ("qna:0123456789AB:1", None),
        (f"qna:{QID}:123", None),
        (f"qna:{QID}:1:x", None),
    ],
    ids=["one-digit", "two-digits", "uppercase-id", "three-digits", "trailing-text"],
)
def test_custom_id_pattern(custom_id, choice):
    m = bridge.QNA_CUSTOM_ID_RE.fullmatch(custom_id)
    assert (m[2] if m else None) == choice


@pytest.mark.parametrize(
    "kwargs",
    [{"type_": "application_command"}, {"custom_id": "ad-status:1"}],
    ids=["not-a-component", "not-a-question-button"],
)
def test_button_ignores_other_interactions(qdir, kwargs):
    i = _interaction(**kwargs)
    assert _button(i) is False
    i.response.send_message.assert_not_awaited()
    i.response.defer.assert_not_awaited()
    i.edit_original_response.assert_not_awaited()


@pytest.mark.parametrize(
    "kwargs, text",
    [
        ({"channel_id": 999}, "Not authorized to answer this question."),
        ({"user_id": 999}, "Not authorized to answer this question."),
        ({"custom_id": f"qna:{QID}:123"}, "Invalid question button. No input sent."),
        ({"message_id": None}, "Invalid question button. No input sent."),
    ],
    ids=["wrong-channel", "unauthorized-user", "three-digit-choice", "no-message"],
)
def test_button_refusals_are_ephemeral(qdir, kwargs, text):
    i = _interaction(**kwargs)
    assert _button(i) is True
    i.response.send_message.assert_awaited_once_with(text, ephemeral=True)
    i.response.defer.assert_not_awaited()


@pytest.mark.parametrize(
    "body, shown",
    [
        (HANDLED, ANSWER),
        (UNHANDLED, "Question record unavailable. No input sent."),
        ("sys.exit(3)", UNCONFIRMED),
        (NO_RESPONSE, UNCONFIRMED),
        (NULL_RESPONSE, UNCONFIRMED),
        (None, UNCONFIRMED),
    ],
    ids=["handled", "unhandled", "router-fails", "no-response", "null-response", "no-router-file"],
)
def test_button_routes_and_edits_response(qdir, body, shown):
    if body is not None:
        _router(qdir, body)
    i = _interaction()
    assert _button(i) is True
    i.response.defer.assert_awaited_once_with(ephemeral=True, thinking=True)
    i.edit_original_response.assert_awaited_once_with(content=shown, allowed_mentions=NO_MENTIONS)


@pytest.mark.parametrize(
    "kwargs, router",
    [
        ({"reference_id": None}, HANDLED),
        ({"channel_id": 999}, HANDLED),
        ({"author_id": 999}, HANDLED),
        ({}, None),
    ],
    ids=["not-a-reply", "wrong-channel", "unauthorized-author", "no-router-file"],
)
def test_reply_falls_through_silently(qdir, caplog, kwargs, router):
    if router is not None:
        _router(qdir, router)
    m = _message(**kwargs)
    assert _reply(m) is False
    m.reply.assert_not_awaited()
    assert [r for r in caplog.records if r.name == "conductor-bridge"] == []


@pytest.mark.parametrize(
    "body, consumed, replies",
    [
        (HANDLED, True, [mock.call(ANSWER, mention_author=False, allowed_mentions=NO_MENTIONS)]),
        (UNHANDLED, False, []),
        ("sys.exit(3)", True, [mock.call(UNAVAILABLE, mention_author=False)]),
        ('print("not json")', True, [mock.call(UNAVAILABLE, mention_author=False)]),
        (NO_RESPONSE, True, [mock.call(UNAVAILABLE, mention_author=False)]),
        (NULL_RESPONSE, True, [mock.call(UNAVAILABLE, mention_author=False)]),
    ],
    ids=["handled", "unhandled", "router-fails", "bad-json", "no-response", "null-response"],
)
def test_reply_routes(qdir, body, consumed, replies):
    _router(qdir, body)
    m = _message()
    assert _reply(m) is consumed
    assert m.reply.await_args_list == replies


def test_reply_send_failure_after_handled_is_not_reported_as_unforwarded(qdir):
    # The router has already acted, so a failed send must surface as itself. Were the
    # send inside the routing try, the failure would be caught and a second reply (the
    # "nothing forwarded" notice) would go out.
    _router(qdir, HANDLED)
    m = _message()
    m.reply.side_effect = [RuntimeError("send failed"), None]
    with pytest.raises(RuntimeError, match="send failed"):
        _reply(m)
    assert m.reply.await_count == 1


def test_events_match_design_shapes(qdir):
    _router(qdir, RECORDER)
    recorded = qdir / "director" / "event.json"
    _button(_interaction())
    assert json.loads(recorded.read_text()) == {
        "reference": "444",
        "message": "555",
        "author": "333",
        "channel": "222",
        "text": "2",
        "custom_id": f"qna:{QID}:2",
    }
    _reply(_message())
    assert json.loads(recorded.read_text()) == {
        "reference": "666",
        "message": "777",
        "author": "333",
        "channel": "222",
        "text": "<@999> 2",
    }


# create_discord_bot reads HAS_DISCORD, discord and app_commands as module globals
# at call time, so a stub builds the real bot offline.
def _keep(**_): return lambda fn: fn  # bot.tree.command(...) and app_commands.describe(...) as no-op decorators
class _Client:  # enough of discord.Client; user None makes message_mentions_bot() False
    user = None
    def __init__(self, intents=None): pass
    def event(self, coro): setattr(self, coro.__name__, coro); return coro
@pytest.mark.parametrize("mode", ["all", "mentions", "mentions_all_channels"])
def test_discord_bot_routes_questions_in_every_listen_mode(qdir, monkeypatch, mode):
    vars(bridge.discord).update(Intents=SimpleNamespace(default=SimpleNamespace), Client=_Client, Object=SimpleNamespace)
    monkeypatch.setattr(bridge, "app_commands", SimpleNamespace(CommandTree=lambda bot: SimpleNamespace(command=_keep),
                                                                describe=_keep), raising=False)
    monkeypatch.setattr(bridge, "HAS_DISCORD", True)
    _router(qdir, HANDLED)
    bot, _ = bridge.create_discord_bot({"discord": {"configured": True, "bot_token": "x", "guild_id": 1,
                                                    "channel_id": CHANNEL, "user_id": USER, "listen_mode": mode}})
    interaction, message = _interaction(), _message()
    asyncio.run(bot.on_interaction(interaction))
    asyncio.run(bot.on_message(message))
    interaction.edit_original_response.assert_awaited_once_with(content=ANSWER, allowed_mentions=NO_MENTIONS)
    message.reply.assert_awaited_once_with(ANSWER, mention_author=False, allowed_mentions=NO_MENTIONS)
