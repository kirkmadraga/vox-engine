package discordio

import (
	"context"
	"errors"
	"fmt"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
)

// ReachRest is the subset of disgo's REST client that checks whether a
// reminder can still reach its owner.
type ReachRest interface {
	GetChannel(channelID snowflake.ID, opts ...rest.RequestOpt) (discord.Channel, error)
	GetMember(guildID, userID snowflake.ID, opts ...rest.RequestOpt) (*discord.Member, error)
	GetGuild(guildID snowflake.ID, withCounts bool, opts ...rest.RequestOpt) (*discord.RestGuild, error)
	GetThreadMember(threadID, userID snowflake.ID, withMember bool, opts ...rest.RequestOpt) (*discord.ThreadMember, error)
}

// Reachable asks Discord whether user can still see channel in guild: the
// channel exists there, user is still a member, and has View Channel in it
// (for a thread, in its parent channel; for a private thread, they must also
// still be in it). ok false with a nil error is a sure no; an error means
// Discord couldn't say. The bot keeps no member or role cache, so this is
// three REST calls (one or two more for a thread).
func Reachable(r ReachRest) func(ctx context.Context, user, guild, channel snowflake.ID) (bool, error) {
	return func(ctx context.Context, user, guild, channel snowflake.ID) (bool, error) {
		opt := rest.WithCtx(ctx)
		ch, err := r.GetChannel(channel, opt)
		if err != nil {
			// A channel of a server the bot was removed from is "Unknown
			// Channel" (checked live, 2026-10-20). Should Discord answer
			// "Missing Access" instead, the server tells the two apart: gone
			// for the bot is a sure no; otherwise it's the bot's own missing
			// permission on the channel, and nobody can say.
			var re *rest.Error
			if errors.As(err, &re) && re.Code == rest.JSONErrorCodeMissingAccess {
				if _, gerr := r.GetGuild(guild, false, opt); gerr != nil && errors.As(gerr, &re) &&
					(re.Code == rest.JSONErrorCodeUnknownGuild || re.Code == rest.JSONErrorCodeMissingAccess) {
					return false, nil
				}
			}
			return gone(err, rest.JSONErrorCodeUnknownChannel)
		}
		gc, ok := ch.(discord.GuildChannel)
		if !ok || gc.GuildID() != guild {
			return false, nil // not a channel of that server (any more)
		}
		if thread, isThread := ch.(discord.GuildThread); isThread && thread.ParentID() != nil {
			// A thread takes its permissions from the channel it's in.
			parent, err := r.GetChannel(*thread.ParentID(), opt)
			if err != nil {
				return gone(err, rest.JSONErrorCodeUnknownChannel)
			}
			if gc, ok = parent.(discord.GuildChannel); !ok {
				return false, nil
			}
		}
		member, err := r.GetMember(guild, user, opt)
		if err != nil {
			return gone(err, rest.JSONErrorCodeUnknownMember)
		}
		g, err := r.GetGuild(guild, false, opt)
		if err != nil {
			return gone(err, rest.JSONErrorCodeUnknownGuild)
		}
		perms := ChannelPermissions(g.OwnerID, g.Roles, *member, gc)
		if !perms.Has(discord.PermissionViewChannel) {
			return false, nil
		}
		if ch.Type() == discord.ChannelTypeGuildPrivateThread && !perms.Has(discord.PermissionManageThreads) {
			// Seeing the parent isn't enough: they must still be in the thread
			// (a mention there could add them back), unless they manage threads
			// there, which lets them see private threads they aren't in.
			if _, err := r.GetThreadMember(channel, user, false, opt); err != nil {
				return gone(err, rest.JSONErrorCodeUnknownMember)
			}
		}
		return true, nil
	}
}

// gone turns a REST error into Reachable's answer: Discord's "unknown"
// code for the thing looked up (or an unknown server) is a sure no; anything
// else, an outage or a missing permission of the bot's own, is "couldn't say".
func gone(err error, unknown rest.JSONErrorCode) (bool, error) {
	var re *rest.Error
	if errors.As(err, &re) && (re.Code == unknown || re.Code == rest.JSONErrorCodeUnknownGuild) {
		return false, nil
	}
	return false, fmt.Errorf("check reminder reach: %w", err)
}

// CanView reports whether member has View Channel in channel.
func CanView(owner snowflake.ID, roles []discord.Role, member discord.Member, channel discord.GuildChannel) bool {
	return ChannelPermissions(owner, roles, member, channel).Has(discord.PermissionViewChannel)
}

// ChannelPermissions is member's permissions in channel, in the order Discord
// documents: the server owner and administrators have all of them; otherwise
// @everyone's and the member's roles' permissions, then the channel's
// overwrites for @everyone, then the member's roles together (denies, then
// allows), then the member's own.
func ChannelPermissions(owner snowflake.ID, roles []discord.Role, member discord.Member, channel discord.GuildChannel) discord.Permissions {
	if member.User.ID == owner {
		return discord.PermissionsAll
	}
	guild := channel.GuildID()
	has := map[snowflake.ID]bool{guild: true} // @everyone's role ID is the server's
	for _, id := range member.RoleIDs {
		has[id] = true
	}
	var perms discord.Permissions
	for _, role := range roles {
		if has[role.ID] {
			perms = perms.Add(role.Permissions)
		}
	}
	if perms.Has(discord.PermissionAdministrator) {
		return discord.PermissionsAll
	}
	overwrites := channel.PermissionOverwrites()
	if o, ok := overwrites.Role(guild); ok {
		perms = perms.Remove(o.Deny).Add(o.Allow)
	}
	var allow, deny discord.Permissions
	for _, id := range member.RoleIDs {
		if id == guild {
			continue
		}
		if o, ok := overwrites.Role(id); ok {
			allow, deny = allow.Add(o.Allow), deny.Add(o.Deny)
		}
	}
	perms = perms.Remove(deny).Add(allow)
	// The member's own overwrite comes last, so it beats their roles' (here
	// Discord's documented order differs from disgo's cache, which merges them).
	if o, ok := overwrites.Member(member.User.ID); ok {
		perms = perms.Remove(o.Deny).Add(o.Allow)
	}
	return perms
}
