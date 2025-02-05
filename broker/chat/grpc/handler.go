/*
 * Copyright (c) 2019-2021. Abstrium SAS <team (at) pydio.com>
 * This file is part of Pydio Cells.
 *
 * Pydio Cells is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * Pydio Cells is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with Pydio Cells.  If not, see <http://www.gnu.org/licenses/>.
 *
 * The latest code can be found at <https://pydio.com>.
 */

package grpc

import (
	"context"
	"errors"
	"fmt"

	"github.com/pydio/cells/v4/common/client/grpc"

	"go.uber.org/zap"

	chat2 "github.com/pydio/cells/v4/broker/chat"
	"github.com/pydio/cells/v4/common"
	"github.com/pydio/cells/v4/common/broker"
	"github.com/pydio/cells/v4/common/log"
	"github.com/pydio/cells/v4/common/proto/chat"
	"github.com/pydio/cells/v4/common/proto/tree"
	"github.com/pydio/cells/v4/common/service/context/metadata"
)

var (
	metaClient tree.NodeReceiverClient
)

func getMetaClient(runtimeContext context.Context) tree.NodeReceiverClient {
	if metaClient == nil {
		metaClient = tree.NewNodeReceiverClient(grpc.GetClientConnFromCtx(runtimeContext, common.ServiceMeta))
	}
	return metaClient
}

type ChatHandler struct {
	chat.UnimplementedChatServiceServer
	RuntimeCtx context.Context
	dao        chat2.DAO
}

func (c *ChatHandler) Name() string {
	return ServiceName
}

func (c *ChatHandler) PutRoom(ctx context.Context, req *chat.PutRoomRequest) (*chat.PutRoomResponse, error) {

	resp := &chat.PutRoomResponse{}
	newRoom, err := c.dao.PutRoom(ctx, req.Room)
	if err != nil {
		return resp, err
	}
	resp.Room = newRoom
	log.Logger(ctx).Debug("Put Room", newRoom.Zap())
	broker.MustPublish(ctx, common.TopicChatEvent, &chat.ChatEvent{
		Room:    resp.Room,
		Details: "PUT",
	})
	return resp, err
}

func (c *ChatHandler) DeleteRoom(ctx context.Context, req *chat.DeleteRoomRequest) (*chat.DeleteRoomResponse, error) {

	response := &chat.DeleteRoomResponse{}

	log.Logger(ctx).Debug("Delete Room", req.Room.Zap())

	ok, err := c.dao.DeleteRoom(ctx, req.Room)
	if err != nil {
		return nil, err
	} else if !ok {
		// should never happen
		return nil, errors.New("cannot delete room, but DeleteRoom method returned no error")
	}

	response.Success = true
	broker.MustPublish(ctx, common.TopicChatEvent, &chat.ChatEvent{
		Room:    req.Room,
		Details: "DELETE",
	})
	return response, nil
}

func (c *ChatHandler) ListRooms(req *chat.ListRoomsRequest, streamer chat.ChatService_ListRoomsServer) error {

	ctx := streamer.Context()
	log.Logger(ctx).Debug("List Rooms", zap.Any(common.KeyChatListRoomReq, req))

	rooms, err := c.dao.ListRooms(ctx, req)
	if err != nil {
		return err
	}
	//defer streamer.Close()
	for _, r := range rooms {
		streamer.Send(&chat.ListRoomsResponse{Room: r})
	}

	return nil
}

func (c *ChatHandler) ListMessages(req *chat.ListMessagesRequest, streamer chat.ChatService_ListMessagesServer) error {

	ctx := streamer.Context()
	log.Logger(ctx).Debug("List Messages", zap.Any(common.KeyChatListMsgReq, req))

	messages, err := c.dao.ListMessages(ctx, req)
	if err != nil {
		return err
	}
	for _, m := range messages {
		streamer.Send(&chat.ListMessagesResponse{Message: m})
	}

	return nil
}

func (c *ChatHandler) PostMessage(ctx context.Context, req *chat.PostMessageRequest) (*chat.PostMessageResponse, error) {

	resp := &chat.PostMessageResponse{}
	log.Logger(ctx).Debug("Post Messages", zap.Any(common.KeyChatPostMsgReq, req))

	for _, m := range req.Messages {
		newMessage, err := c.dao.PostMessage(ctx, m)
		if err != nil {
			return nil, err
		}
		resp.Messages = append(resp.Messages, newMessage)
	}
	resp.Success = true
	go func() {
		for _, m := range resp.Messages {
			bgCtx := metadata.NewBackgroundWithUserKey(m.Author)
			broker.MustPublish(bgCtx, common.TopicChatEvent, &chat.ChatEvent{
				Message: m,
			})
			// For comments on nodes, publish an UPDATE_USER_META event
			if room, err := c.dao.RoomByUuid(bgCtx, chat.RoomType_NODE, m.RoomUuid); err == nil {
				broker.MustPublish(bgCtx, common.TopicMetaChanges, &tree.NodeChangeEvent{
					Type: tree.NodeChangeEvent_UPDATE_USER_META,
					Target: &tree.Node{Uuid: room.RoomTypeObject, MetaStore: map[string]string{
						"comments": `"` + m.Message + `"`,
					}},
				})
				if count, e := c.dao.CountMessages(bgCtx, room); e == nil {
					getMetaClient(c.RuntimeCtx).UpdateNode(bgCtx, &tree.UpdateNodeRequest{To: &tree.Node{
						Uuid: room.RoomTypeObject,
						MetaStore: map[string]string{
							"has_comments": fmt.Sprintf("%d", count),
						},
					}})
				}
			}
		}
	}()
	return resp, nil
}

func (c *ChatHandler) DeleteMessage(ctx context.Context, req *chat.DeleteMessageRequest) (*chat.DeleteMessageResponse, error) {

	log.Logger(ctx).Debug("Delete Messages", zap.Any(common.KeyChatPostMsgReq, req))

	for _, m := range req.Messages {
		err := c.dao.DeleteMessage(ctx, m)
		if err != nil {
			return nil, err
		}
		broker.MustPublish(ctx, common.TopicChatEvent, &chat.ChatEvent{
			Message: m,
			Details: "DELETE",
		})
	}
	go func() {
		for _, m := range req.Messages {
			bgCtx := metadata.NewBackgroundWithUserKey(m.Author)
			if room, err := c.dao.RoomByUuid(bgCtx, chat.RoomType_NODE, m.RoomUuid); err == nil {
				if count, e := c.dao.CountMessages(bgCtx, room); e == nil {
					var meta = ""
					if count > 0 {
						meta = fmt.Sprintf("%d", count)
					}
					getMetaClient(c.RuntimeCtx).UpdateNode(bgCtx, &tree.UpdateNodeRequest{To: &tree.Node{
						Uuid: room.RoomTypeObject,
						MetaStore: map[string]string{
							"has_comments": meta,
						},
					}})
				}
			}
		}
	}()
	return &chat.DeleteMessageResponse{Success: true}, nil
}
