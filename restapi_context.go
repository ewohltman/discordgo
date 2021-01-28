package discordgo

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// RequestWithContext is the same as RequestWithContextBucketID but the bucket
// ID is the same as urlStr.
func (s *Session) RequestWithContext(ctx context.Context, method, urlStr string, data interface{}) (response []byte, err error) {
	return s.RequestWithContextBucketID(ctx, method, urlStr, data, strings.SplitN(urlStr, "?", 2)[0])
}

// RequestWithContextBucketID makes a (GET/POST/...) request to the Discord
// REST API with JSON data using the provided context.
func (s *Session) RequestWithContextBucketID(ctx context.Context, method, urlStr string, data interface{}, bucketID string) (response []byte, err error) {
	var body []byte

	if data != nil {
		body, err = json.Marshal(data)
		if err != nil {
			return
		}
	}

	return s.request(ctx, method, urlStr, "application/json", body, bucketID, 0)
}

// RequestWithContextLockedBucket makes a request using a bucket that's already
// been locked using the provided context.
func (s *Session) RequestWithContextLockedBucket(ctx context.Context, method, urlStr, contentType string, b []byte, bucket *Bucket, sequence int) (response []byte, err error) {
	if s.Debug {
		log.Printf("API REQUEST %8s :: %s\n", method, urlStr)
		log.Printf("API REQUEST  PAYLOAD :: [%s]\n", string(b))
	}

	req, err := http.NewRequestWithContext(ctx, method, urlStr, bytes.NewBuffer(b))
	if err != nil {
		bucket.Release(nil)
		return
	}

	// Not used on initial login..
	// TODO: Verify if a login, otherwise complain about no-token
	if s.Token != "" {
		req.Header.Set("authorization", s.Token)
	}

	// Discord's API returns a 400 Bad Request is Content-Type is set, but the
	// request body is empty.
	if b != nil {
		req.Header.Set("Content-Type", contentType)
	}

	// TODO: Make a configurable static variable.
	req.Header.Set("User-Agent", s.UserAgent)

	if s.Debug {
		for k, v := range req.Header {
			log.Printf("API REQUEST   HEADER :: [%s] = %+v\n", k, v)
		}
	}

	resp, err := s.Client.Do(req)
	if err != nil {
		bucket.Release(nil)
		return
	}
	defer func() {
		err2 := resp.Body.Close()
		if err2 != nil {
			log.Println("error closing resp body")
		}
	}()

	err = bucket.Release(resp.Header)
	if err != nil {
		return
	}

	response, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		return
	}

	if s.Debug {
		log.Printf("API RESPONSE  STATUS :: %s\n", resp.Status)
		for k, v := range resp.Header {
			log.Printf("API RESPONSE  HEADER :: [%s] = %+v\n", k, v)
		}
		log.Printf("API RESPONSE    BODY :: [%s]\n\n\n", response)
	}

	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusNoContent:
		return
	case http.StatusInternalServerError, http.StatusBadGateway:
		// Retry sending request if possible
		if sequence >= s.MaxRestRetries {
			err = fmt.Errorf("exceeded max HTTP retries: %s, %s", resp.Status, response)
			return
		}

		s.log(LogDebug, "%s Failed (%s), Retrying...", urlStr, resp.Status)

		err = s.Ratelimiter.LockBucketObject(ctx, bucket)
		if err != nil {
			return
		}

		response, err = s.RequestWithContextLockedBucket(ctx, method, urlStr, contentType, b, bucket, sequence+1)
	case http.StatusTooManyRequests: // Rate limiting
		// Retry sending request if possible
		if sequence >= s.MaxRestRetries {
			err = fmt.Errorf("exceeded max HTTP retries: %s, %s", resp.Status, response)
			return
		}

		rl := TooManyRequests{}

		err = json.Unmarshal(response, &rl)
		if err != nil {
			err = fmt.Errorf("rate limit unmarshal error: %s", err)
			s.log(LogError, err.Error())
			return
		}

		var retryAfter time.Duration

		retryAfter, err = time.ParseDuration(fmt.Sprintf("%fs", rl.RetryAfter))
		if err != nil {
			err = fmt.Errorf("rate limit parsing error: %s", err)
			s.log(LogError, err.Error())
			return
		}

		s.log(LogWarning, "Rate limiting %s: retry in %f s", urlStr, rl.RetryAfter)
		s.handleEvent(rateLimitEventType, &RateLimit{TooManyRequests: &rl, URL: urlStr})

		retryTime := time.NewTimer(retryAfter)
		defer retryTime.Stop()

		select {
		case <-retryTime.C:
			err = s.Ratelimiter.LockBucketObject(ctx, bucket)
			if err != nil {
				s.log(LogError, err.Error())
				return
			}

			response, err = s.RequestWithContextLockedBucket(ctx, method, urlStr, contentType, b, bucket, sequence)
		case <-ctx.Done():
			err = fmt.Errorf("rate limit error: %w", ctx.Err())
			s.log(LogError, err.Error())
			return
		}
	case http.StatusUnauthorized:
		if strings.Index(s.Identify.Token, "Bot ") != 0 {
			s.log(LogError, ErrUnauthorized.Error())
			err = ErrUnauthorized
		}
		fallthrough
	default: // Error condition
		err = newRestError(req, resp, response)
	}

	return
}

// UserWithContext returns the user details of the given userID via the Discord
// REST API with the given context. A userID of "@me" is a shortcut of current
// user ID.
func (s *Session) UserWithContext(ctx context.Context, userID string) (st *User, err error) {
	body, err := s.RequestWithContextBucketID(ctx, "GET", EndpointUser(userID), nil, EndpointUsers)
	if err != nil {
		return
	}

	err = unmarshal(body, &st)

	return
}

// UserChannelPermissionsWithContext returns the permission of a user in a
// channel.
// userID    : The ID of the user to calculate permissions for.
// channelID : The ID of the channel to calculate permission for.
//
// NOTE: This function is now deprecated and will be removed in the future.
// Please see the same function inside state.go
func (s *Session) UserChannelPermissionsWithContext(ctx context.Context, userID, channelID string) (apermissions int64, err error) {
	// Try to just get permissions from state.
	apermissions, err = s.State.UserChannelPermissions(userID, channelID)
	if err == nil {
		return
	}

	// Otherwise try get as much data from state as possible, falling back to the network.
	channel, err := s.State.Channel(channelID)
	if err != nil || channel == nil {
		channel, err = s.ChannelWithContext(ctx, channelID)
		if err != nil {
			return
		}
	}

	guild, err := s.State.Guild(channel.GuildID)
	if err != nil || guild == nil {
		guild, err = s.GuildWithContext(ctx, channel.GuildID)
		if err != nil {
			return
		}
	}

	if userID == guild.OwnerID {
		apermissions = PermissionAll
		return
	}

	member, err := s.State.Member(guild.ID, userID)
	if err != nil || member == nil {
		member, err = s.GuildMemberWithContext(ctx, guild.ID, userID)
		if err != nil {
			return
		}
	}

	return memberPermissions(guild, channel, userID, member.Roles), nil
}

// GuildWithContext returns a Guild structure of a specific Guild via the
// Discord REST API using the provided context.
func (s *Session) GuildWithContext(ctx context.Context, guildID string) (st *Guild, err error) {
	body, err := s.RequestWithContextBucketID(ctx, "GET", EndpointGuild(guildID), nil, EndpointGuild(guildID))
	if err != nil {
		return
	}

	err = unmarshal(body, &st)

	return
}

// GuildMembersWithContext returns a list of members for a guild using the
// provided context. The after parameter is to denote a guild member user ID to
// return members after. If set to an empty string, it will start from the
// beginning. The limit parameter denotes the max number of members to return.
// The value must be greater than 0 and less than 1000.
func (s *Session) GuildMembersWithContext(ctx context.Context, guildID string, after string, limit int) ([]*Member, error) {
	if limit < 0 {
		return nil, fmt.Errorf("limit (%d) must be greater than 0", limit)
	}

	if limit > 1000 {
		return nil, fmt.Errorf("limit (%d) exceeds max value of 1000", limit)
	}

	urlValues := url.Values{}
	uri := EndpointGuildMembers(guildID)

	if after != "" {
		urlValues.Set("after", after)
	}

	urlValues.Set("limit", strconv.Itoa(limit))

	if len(urlValues) > 0 {
		uri += "?" + urlValues.Encode()
	}

	body, err := s.RequestWithContextBucketID(ctx, http.MethodGet, uri, nil, EndpointGuildMembers(guildID))
	if err != nil {
		return nil, err
	}

	var members []*Member

	err = unmarshal(body, &members)
	if err != nil {
		return nil, err
	}

	return members, nil
}

// GuildMemberWithContext returns a member of a guild via the Discord REST API
// using the given context.
func (s *Session) GuildMemberWithContext(ctx context.Context, guildID, userID string) (st *Member, err error) {
	body, err := s.RequestWithContextBucketID(ctx, "GET", EndpointGuildMember(guildID, userID), nil, EndpointGuildMember(guildID, ""))
	if err != nil {
		return
	}

	err = unmarshal(body, &st)

	return
}

// GuildMemberRoleAddWithContext adds the specified role to a given member via
// the Discord REST API using the provided context.
func (s *Session) GuildMemberRoleAddWithContext(ctx context.Context, guildID, userID, roleID string) (err error) {
	_, err = s.RequestWithContextBucketID(ctx, "PUT", EndpointGuildMemberRole(guildID, userID, roleID), nil, EndpointGuildMemberRole(guildID, "", ""))

	return
}

// GuildMemberRoleRemoveWithContext removes the specified role from a given
// member via the Discord REST API using the provided context.
func (s *Session) GuildMemberRoleRemoveWithContext(ctx context.Context, guildID, userID, roleID string) (err error) {
	_, err = s.RequestWithContextBucketID(ctx, "DELETE", EndpointGuildMemberRole(guildID, userID, roleID), nil, EndpointGuildMemberRole(guildID, "", ""))

	return
}

// GuildChannelsWithContext returns a slice of Channel structures for all
// channels of a given guild via the Discord REST API.using the provided context.
func (s *Session) GuildChannelsWithContext(ctx context.Context, guildID string) (st []*Channel, err error) {
	body, err := s.request(ctx, "GET", EndpointGuildChannels(guildID), "", nil, EndpointGuildChannels(guildID), 0)
	if err != nil {
		return
	}

	err = unmarshal(body, &st)

	return
}

// GuildRolesWithContext returns all roles for a given guild via the Discord
// REST API. using the given context.
func (s *Session) GuildRolesWithContext(ctx context.Context, guildID string) (st Roles, err error) {
	body, err := s.RequestWithContextBucketID(ctx, "GET", EndpointGuildRoles(guildID), nil, EndpointGuildRoles(guildID))
	if err != nil {
		return
	}

	err = unmarshal(body, &st)

	return
}

// GuildRoleCreateWithContext returns a new Guild Role via the Discord REST API
// using the provided context.
func (s *Session) GuildRoleCreateWithContext(ctx context.Context, guildID string) (st *Role, err error) {
	body, err := s.RequestWithContextBucketID(ctx, "POST", EndpointGuildRoles(guildID), nil, EndpointGuildRoles(guildID))
	if err != nil {
		return
	}

	err = unmarshal(body, &st)

	return
}

// GuildRoleEditWithContext updates an existing Guild Role with new values via
// the Discord REST API using the provided context. The color parameter must be
// provided as a decimal value, not hex. The hoist parameter is for whether to
// display the role's users separately. The perm parameter is the permissions
// for the role. The mention parameter is for whether this role is mentionable.
func (s *Session) GuildRoleEditWithContext(ctx context.Context, guildID, roleID, name string, color int, hoist bool, perm int64, mention bool) (st *Role, err error) {
	// Prevent sending a color int that is too big.
	if color > 0xFFFFFF {
		err = fmt.Errorf("color value cannot be larger than 0xFFFFFF")
		return nil, err
	}

	data := struct {
		Name        string `json:"name"`               // The role's name (overwrites existing)
		Color       int    `json:"color"`              // The color the role should have (as a decimal, not hex)
		Hoist       bool   `json:"hoist"`              // Whether to display the role's users separately
		Permissions int64  `json:"permissions,string"` // The overall permissions number of the role (overwrites existing)
		Mentionable bool   `json:"mentionable"`        // Whether this role is mentionable
	}{name, color, hoist, perm, mention}

	body, err := s.RequestWithContextBucketID(ctx, "PATCH", EndpointGuildRole(guildID, roleID), data, EndpointGuildRole(guildID, ""))
	if err != nil {
		return
	}

	err = unmarshal(body, &st)

	return
}

// GuildRoleDeleteWithContext deletes an existing role.
// guildID   : The ID of a Guild.
// roleID    : The ID of a Role.
func (s *Session) GuildRoleDeleteWithContext(ctx context.Context, guildID, roleID string) (err error) {
	_, err = s.RequestWithContextBucketID(
		ctx,
		http.MethodDelete,
		EndpointGuildRole(guildID, roleID),
		nil,
		EndpointGuildRole(guildID, ""),
	)

	return
}

// ChannelWithContext returns a Channel structure of a specific Channel via the
// Discord REST API using the provided context.
func (s *Session) ChannelWithContext(ctx context.Context, channelID string) (st *Channel, err error) {
	body, err := s.RequestWithContextBucketID(ctx, "GET", EndpointChannel(channelID), nil, EndpointChannel(channelID))
	if err != nil {
		return
	}

	err = unmarshal(body, &st)

	return
}

// ChannelMessageSendComplexWithContext sends a message to the given channel
// via the Discord REST API using the provided context.
func (s *Session) ChannelMessageSendComplexWithContext(ctx context.Context, channelID string, data *MessageSend) (st *Message, err error) {
	if data.Embed != nil && data.Embed.Type == "" {
		data.Embed.Type = "rich"
	}

	endpoint := EndpointChannelMessages(channelID)

	// TODO: Remove this when compatibility is not required.
	files := data.Files
	if data.File != nil {
		if files == nil {
			files = []*File{data.File}
		} else {
			err = fmt.Errorf("cannot specify both File and Files")
			return
		}
	}

	var response []byte
	if len(files) > 0 {
		body := &bytes.Buffer{}
		bodywriter := multipart.NewWriter(body)

		var payload []byte
		payload, err = json.Marshal(data)
		if err != nil {
			return
		}

		var p io.Writer

		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition", `form-data; name="payload_json"`)
		h.Set("Content-Type", "application/json")

		p, err = bodywriter.CreatePart(h)
		if err != nil {
			return
		}

		if _, err = p.Write(payload); err != nil {
			return
		}

		for i, file := range files {
			h := make(textproto.MIMEHeader)
			h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file%d"; filename="%s"`, i, quoteEscaper.Replace(file.Name)))
			contentType := file.ContentType
			if contentType == "" {
				contentType = "application/octet-stream"
			}
			h.Set("Content-Type", contentType)

			p, err = bodywriter.CreatePart(h)
			if err != nil {
				return
			}

			if _, err = io.Copy(p, file.Reader); err != nil {
				return
			}
		}

		err = bodywriter.Close()
		if err != nil {
			return
		}

		response, err = s.request(ctx, "POST", endpoint, bodywriter.FormDataContentType(), body.Bytes(), endpoint, 0)
	} else {
		response, err = s.RequestWithContextBucketID(ctx, "POST", endpoint, data, endpoint)
	}
	if err != nil {
		return
	}

	err = unmarshal(response, &st)

	return
}
