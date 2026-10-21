package discordio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
)

const (
	reachGuild  snowflake.ID = 100
	reachOwner  snowflake.ID = 1 // the server's owner
	reachUser   snowflake.ID = 8
	roleFriends snowflake.ID = 300
	roleMuted   snowflake.ID = 301
	roleAdmin   snowflake.ID = 302
)

var view = discord.PermissionViewChannel

func everyone(p discord.Permissions) discord.Role {
	return discord.Role{ID: reachGuild, Permissions: p}
}

func role(id snowflake.ID, p discord.Permissions) discord.Role {
	return discord.Role{ID: id, Permissions: p}
}

// channelFrom decodes a channel as Discord sends it (disgo's channel types
// have unexported fields).
func channelFrom(js string) discord.Channel {
	var u discord.UnmarshalChannel
	if err := json.Unmarshal([]byte(js), &u); err != nil {
		panic(err)
	}
	return u.Channel
}

// textIn is text channel 500 in guild, with the given overwrites (JSON).
func textIn(guild snowflake.ID, overwrites ...string) discord.GuildChannel {
	return channelFrom(fmt.Sprintf(`{"id":"500","type":0,"guild_id":"%d","permission_overwrites":[%s]}`, guild, strings.Join(overwrites, ","))).(discord.GuildChannel)
}

func text(overwrites ...string) discord.GuildChannel { return textIn(reachGuild, overwrites...) }

// roleOw and memberOw are permission overwrites (type 0: role, 1: member).
func roleOw(id snowflake.ID, allow, deny discord.Permissions) string {
	return fmt.Sprintf(`{"id":"%d","type":0,"allow":"%d","deny":"%d"}`, id, allow, deny)
}

func memberOw(id snowflake.ID, allow, deny discord.Permissions) string {
	return fmt.Sprintf(`{"id":"%d","type":1,"allow":"%d","deny":"%d"}`, id, allow, deny)
}

func member(id snowflake.ID, roles ...snowflake.ID) discord.Member {
	return discord.Member{User: discord.User{ID: id}, GuildID: reachGuild, RoleIDs: roles}
}

// CanView follows Discord's rules: server permissions from @everyone and
// the member's roles, then the channel's overwrites (@everyone, then the
// roles' denies and allows together, then the member's).
func TestCanView(t *testing.T) {
	roles := []discord.Role{everyone(view), role(roleFriends, view), role(roleMuted, 0), role(roleAdmin, discord.PermissionAdministrator)}
	noView := []discord.Role{everyone(0), role(roleFriends, view), role(roleMuted, 0), role(roleAdmin, discord.PermissionAdministrator)}
	cases := []struct {
		name    string
		roles   []discord.Role
		member  discord.Member
		channel discord.GuildChannel
		want    bool
	}{
		{"everyone can view", roles, member(reachUser), text(), true},
		{"nobody can view by default", noView, member(reachUser), text(), false},
		{"a role grants it", noView, member(reachUser, roleFriends), text(), true},
		{"a role the member doesn't have doesn't count", noView, member(reachUser, roleMuted), text(), false},
		{"private channel: @everyone denied", roles, member(reachUser), text(roleOw(reachGuild, 0, view)), false},
		{"private channel, but the member's role is allowed", roles, member(reachUser, roleFriends),
			text(roleOw(reachGuild, 0, view), roleOw(roleFriends, view, 0)), true},
		{"a role's allow beats another role's deny", roles, member(reachUser, roleFriends, roleMuted),
			text(roleOw(roleMuted, 0, view), roleOw(roleFriends, view, 0)), true},
		{"a role denies it", roles, member(reachUser, roleMuted), text(roleOw(roleMuted, 0, view)), false},
		{"the member's own deny beats a role's allow", roles, member(reachUser, roleFriends),
			text(roleOw(roleFriends, view, 0), memberOw(reachUser, 0, view)), false},
		{"the member's own allow beats a role's deny", roles, member(reachUser, roleMuted),
			text(roleOw(roleMuted, 0, view), memberOw(reachUser, view, 0)), true},
		{"@everyone allowed on the channel", noView, member(reachUser), text(roleOw(reachGuild, view, 0)), true},
		{"another member's overwrite doesn't count", roles, member(reachUser), text(roleOw(reachGuild, 0, view), memberOw(9, view, 0)), false},
		{"an administrator sees everything", roles, member(reachUser, roleAdmin), text(memberOw(reachUser, 0, view)), true},
		{"so does the server's owner", noView, member(reachOwner), text(roleOw(reachGuild, 0, view), memberOw(reachOwner, 0, view)), true},
	}
	for _, c := range cases {
		if got := CanView(reachOwner, c.roles, c.member, c.channel); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// fakeReach is a ReachRest serving one channel (and maybe a thread in it),
// one member and one guild, with errors where set.
type fakeReach struct {
	channels  map[snowflake.ID]discord.Channel
	member    *discord.Member
	guild     *discord.RestGuild
	chErr     error
	memberErr error
	guildErr  error
	threadErr error // GetThreadMember's answer (nil: they're in the thread)
	threadAsk int   // GetThreadMember calls
	calls     int
}

func (f *fakeReach) GetThreadMember(_, _ snowflake.ID, _ bool, _ ...rest.RequestOpt) (*discord.ThreadMember, error) {
	f.calls++
	f.threadAsk++
	if f.threadErr != nil {
		return nil, f.threadErr
	}
	return &discord.ThreadMember{}, nil
}

func (f *fakeReach) GetChannel(id snowflake.ID, _ ...rest.RequestOpt) (discord.Channel, error) {
	f.calls++
	if f.chErr != nil {
		return nil, f.chErr
	}
	if c, ok := f.channels[id]; ok {
		return c, nil
	}
	return nil, &rest.Error{Code: rest.JSONErrorCodeUnknownChannel}
}

func (f *fakeReach) GetMember(_, _ snowflake.ID, _ ...rest.RequestOpt) (*discord.Member, error) {
	f.calls++
	if f.memberErr != nil {
		return nil, f.memberErr
	}
	return f.member, nil
}

func (f *fakeReach) GetGuild(_ snowflake.ID, _ bool, _ ...rest.RequestOpt) (*discord.RestGuild, error) {
	f.calls++
	if f.guildErr != nil {
		return nil, f.guildErr
	}
	return f.guild, nil
}

func newFakeReach(channel discord.GuildChannel) *fakeReach {
	m := member(reachUser)
	return &fakeReach{
		channels: map[snowflake.ID]discord.Channel{channel.ID(): channel},
		member:   &m,
		guild:    &discord.RestGuild{Guild: discord.Guild{ID: reachGuild, OwnerID: reachOwner}, Roles: []discord.Role{everyone(view)}},
	}
}

func reach(t *testing.T, f *fakeReach, channel snowflake.ID) (bool, error) {
	t.Helper()
	return Reachable(f)(context.Background(), reachUser, reachGuild, channel)
}

func TestReachable(t *testing.T) {
	code := func(c rest.JSONErrorCode) error { return &rest.Error{Code: c} }
	if ok, err := reach(t, newFakeReach(text()), 500); !ok || err != nil {
		t.Errorf("a member who can see it: %v, %v", ok, err)
	}
	// Sure noes: deleted channel, a channel of another server, a kicked
	// member, a server the bot was removed from, and a private channel. The
	// codes are what Discord answered when checked live (2026-10-20): for a
	// server the bot isn't in, the channel lookup says Unknown Channel, and the
	// member and server lookups Unknown Guild.
	for name, setup := range map[string]func(f *fakeReach){
		"channel deleted, or in a server the bot left": func(f *fakeReach) { f.chErr = code(rest.JSONErrorCodeUnknownChannel) },
		"channel of another server":                    func(f *fakeReach) { f.channels[500] = textIn(999) },
		"not a server channel":                         func(f *fakeReach) { f.channels[500] = channelFrom(`{"id":"500","type":1}`) },
		"kicked":                                       func(f *fakeReach) { f.memberErr = code(rest.JSONErrorCodeUnknownMember) },
		"bot left the server (member lookup)":          func(f *fakeReach) { f.memberErr = code(rest.JSONErrorCodeUnknownGuild) },
		"bot left the server (server lookup)":          func(f *fakeReach) { f.guildErr = code(rest.JSONErrorCodeUnknownGuild) },
		"can't see the channel":                        func(f *fakeReach) { f.channels[500] = text(roleOw(reachGuild, 0, view)) },
		// Should the channel lookup ever say Missing Access, the server lookup decides.
		"missing access, and the bot is out of the server": func(f *fakeReach) {
			f.chErr, f.guildErr = code(rest.JSONErrorCodeMissingAccess), code(rest.JSONErrorCodeUnknownGuild)
		},
		"missing access on both": func(f *fakeReach) {
			f.chErr, f.guildErr = code(rest.JSONErrorCodeMissingAccess), code(rest.JSONErrorCodeMissingAccess)
		},
	} {
		f := newFakeReach(text())
		setup(f)
		if ok, err := reach(t, f, 500); ok || err != nil {
			t.Errorf("%s: %v, %v; want a sure no", name, ok, err)
		}
	}
	// Couldn't say: outages, and the bot's own missing access to a channel
	// of a server it's still in.
	for name, setup := range map[string]func(f *fakeReach){
		"channel lookup 502":        func(f *fakeReach) { f.chErr = &rest.Error{Response: &http.Response{StatusCode: 502}} },
		"bot can't see the channel": func(f *fakeReach) { f.chErr = code(rest.JSONErrorCodeMissingAccess) },
		"bot can't see the channel, server lookup down": func(f *fakeReach) {
			f.chErr, f.guildErr = code(rest.JSONErrorCodeMissingAccess), errors.New("connection reset")
		},
		"member lookup timeout": func(f *fakeReach) { f.memberErr = context.DeadlineExceeded },
		"guild lookup fails":    func(f *fakeReach) { f.guildErr = errors.New("connection reset") },
	} {
		f := newFakeReach(text())
		setup(f)
		if ok, err := reach(t, f, 500); ok || err == nil {
			t.Errorf("%s: %v, %v; want an error (couldn't say)", name, ok, err)
		}
	}
}

// A thread uses its parent channel's permissions; a deleted parent is a no.
func TestReachableThreads(t *testing.T) {
	thread := channelFrom(`{"id":"600","type":11,"guild_id":"100","parent_id":"500"}`) // a public thread in channel 500
	f := newFakeReach(text(roleOw(reachGuild, 0, view)))                               // the parent is private
	f.channels[600] = thread
	if ok, err := reach(t, f, 600); ok || err != nil {
		t.Errorf("thread in a channel they can't see: %v, %v", ok, err)
	}
	f.channels[500] = text()
	if ok, err := reach(t, f, 600); !ok || err != nil {
		t.Errorf("thread in a channel they can see: %v, %v", ok, err)
	}
	delete(f.channels, 500)
	if ok, err := reach(t, f, 600); ok || err != nil {
		t.Errorf("thread whose channel was deleted: %v, %v", ok, err)
	}
	if f.threadAsk != 0 {
		t.Errorf("a public thread's members were looked up %d times", f.threadAsk)
	}
}

// A private thread: seeing its channel isn't enough; they must still be in it.
func TestReachablePrivateThreads(t *testing.T) {
	f := newFakeReach(text())
	f.channels[600] = channelFrom(`{"id":"600","type":12,"guild_id":"100","parent_id":"500"}`)
	if ok, err := reach(t, f, 600); !ok || err != nil || f.threadAsk != 1 {
		t.Errorf("still in it: %v, %v (thread members asked %d)", ok, err, f.threadAsk)
	}
	f.threadErr = &rest.Error{Code: rest.JSONErrorCodeUnknownMember}
	if ok, err := reach(t, f, 600); ok || err != nil {
		t.Errorf("removed from it: %v, %v; want a sure no", ok, err)
	}
	f.threadErr = &rest.Error{Response: &http.Response{StatusCode: 502}}
	if ok, err := reach(t, f, 600); ok || err == nil {
		t.Errorf("thread lookup down: %v, %v; want couldn't say", ok, err)
	}
	// Managing threads in its channel lets them see a private thread they
	// aren't in: kept, without asking, whether that comes from a role or from
	// an overwrite on the channel.
	f.threadErr = &rest.Error{Code: rest.JSONErrorCodeUnknownMember}
	manage := discord.PermissionManageThreads
	m := member(reachUser, roleFriends)
	f.member = &m
	f.guild.Roles = []discord.Role{everyone(view), role(roleFriends, manage)}
	f.threadAsk = 0
	if ok, err := reach(t, f, 600); !ok || err != nil || f.threadAsk != 0 {
		t.Errorf("manages threads (role): %v, %v (thread members asked %d)", ok, err, f.threadAsk)
	}
	f.guild.Roles = []discord.Role{everyone(view), role(roleFriends, 0)}
	f.channels[500] = text(roleOw(roleFriends, manage, 0))
	if ok, err := reach(t, f, 600); !ok || err != nil || f.threadAsk != 0 {
		t.Errorf("manages threads (channel overwrite): %v, %v (thread members asked %d)", ok, err, f.threadAsk)
	}
	f.channels[500] = text(roleOw(reachGuild, 0, manage)) // denied it here: back to needing membership
	if ok, err := reach(t, f, 600); ok || err != nil || f.threadAsk != 1 {
		t.Errorf("Manage Threads denied on the channel: %v, %v (thread members asked %d)", ok, err, f.threadAsk)
	}
	m = member(reachUser)
	f.member, f.guild.Roles = &m, []discord.Role{everyone(view)}

	// Not asked when the channel already rules them out.
	f.threadErr, f.threadAsk = nil, 0
	f.channels[500] = text(roleOw(reachGuild, 0, view))
	if ok, _ := reach(t, f, 600); ok || f.threadAsk != 0 {
		t.Errorf("can't see its channel: %v, thread members asked %d", ok, f.threadAsk)
	}
}
