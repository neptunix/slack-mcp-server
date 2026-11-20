package handler

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/gocarina/gocsv"
	"github.com/korotovsky/slack-mcp-server/pkg/limiter"
	"github.com/korotovsky/slack-mcp-server/pkg/provider"
	"github.com/korotovsky/slack-mcp-server/pkg/provider/edge"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/slack-go/slack"
	"go.uber.org/zap"
)

type SavedItemRow struct {
	ItemID      string `csv:"ItemID"`
	ChannelID   string `csv:"ChannelID"`
	ChannelName string `csv:"ChannelName"`
	Ts          string `csv:"Ts"`
	DateCreated string `csv:"DateCreated"`
	DateDue     string `csv:"DateDue"`
	State       string `csv:"State"`
	TodoState   string `csv:"TodoState"`
}

// SavedMessageRow is a saved message with its Saved/To-dos metadata.
type SavedMessageRow struct {
	TodoState string `csv:"TodoState"`
	DateDue   string `csv:"DateDue"`
	Message
}

// ValidSavedFilters are the filter values accepted by saved.list.
var ValidSavedFilters = []string{"saved", "saved_no_due_date", "todo", "completed", "archived"}

type SavedHandler struct {
	apiProvider *provider.ApiProvider
	logger      *zap.Logger
	convHandler *ConversationsHandler
}

func NewSavedHandler(apiProvider *provider.ApiProvider, logger *zap.Logger, convHandler *ConversationsHandler) *SavedHandler {
	return &SavedHandler{apiProvider: apiProvider, logger: logger, convHandler: convHandler}
}

func (h *SavedHandler) SavedListHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	h.logger.Debug("SavedListHandler called", zap.Any("params", request.Params))

	filter := request.GetString("filter", "saved")
	if !slices.Contains(ValidSavedFilters, filter) {
		return nil, fmt.Errorf("invalid filter %q, must be one of: %s", filter, strings.Join(ValidSavedFilters, ", "))
	}
	limit := request.GetInt("limit", 50)
	includeMessages := request.GetBool("include_messages", true)
	maxMsgsPerItem := request.GetInt("max_messages_per_item", 5)

	channelsMaps := h.apiProvider.ProvideChannelsMaps()
	workspaceURL := ""
	if includeMessages {
		if ar, err := h.apiProvider.Slack().AuthTest(); err == nil {
			workspaceURL = ar.URL
		}
	}
	rl := limiter.Tier3.Limiter()

	var allItems []SavedItemRow
	var allMessages []SavedMessageRow
	cursor := ""
	fetched := 0
	pageSize := limit
	if pageSize > 50 {
		pageSize = 50
	}

	for fetched < limit {
		if cursor != "" {
			if err := rl.Wait(ctx); err != nil {
				h.logger.Warn("Rate limiter wait failed, stopping pagination", zap.Error(err))
				break
			}
		}

		resp, err := h.apiProvider.Slack().SavedList(ctx, filter, pageSize, cursor)
		if err != nil {
			h.logger.Error("SavedList failed", zap.Error(err))
			return nil, fmt.Errorf("failed to list saved items: %v", err)
		}

		for _, item := range resp.SavedItems {
			if fetched >= limit {
				break
			}

			channelName := item.ItemID
			if cached, ok := channelsMaps.Channels[item.ItemID]; ok {
				channelName = cached.Name
			}

			row := SavedItemRow{
				ItemID:      item.ItemID,
				ChannelID:   item.ItemID,
				ChannelName: channelName,
				Ts:          item.Ts,
				DateCreated: formatUnixTs(item.DateCreated),
				DateDue:     formatUnixTs(item.DateDue),
				State:       item.State,
				TodoState:   item.TodoState,
			}
			allItems = append(allItems, row)
			fetched++

			if includeMessages {
				if err := rl.Wait(ctx); err != nil {
					break
				}
				for _, m := range h.fetchSavedItemMessages(ctx, item, channelName, maxMsgsPerItem) {
					if m.Permalink == "" {
						m.Permalink = savedPermalink(workspaceURL, item.ItemID, m.MsgID, m.ThreadTs)
					}
					allMessages = append(allMessages, SavedMessageRow{TodoState: item.TodoState, DateDue: row.DateDue, Message: m})
				}
			}
		}

		if resp.ResponseMetadata.NextCursor == "" || fetched >= limit {
			break
		}
		cursor = resp.ResponseMetadata.NextCursor
	}

	if includeMessages && len(allMessages) > 0 {
		csvBytes, err := gocsv.MarshalBytes(&allMessages)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal saved messages: %v", err)
		}
		return mcp.NewToolResultText(string(csvBytes)), nil
	}

	csvBytes, err := gocsv.MarshalBytes(&allItems)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal saved items: %v", err)
	}
	return mcp.NewToolResultText(string(csvBytes)), nil
}

func (h *SavedHandler) SavedUpdateHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	h.logger.Debug("SavedUpdateHandler called", zap.Any("params", request.Params))

	itemID := request.GetString("item_id", "")
	ts := request.GetString("ts", "")
	mark := request.GetString("mark", "")
	dateDue := int64(request.GetInt("date_due", 0))

	if itemID == "" || ts == "" {
		return nil, fmt.Errorf("item_id and ts are required parameters")
	}
	if mark == "" && dateDue == 0 {
		return nil, fmt.Errorf("at least one of mark or date_due must be provided")
	}

	err := h.apiProvider.Slack().SavedUpdate(ctx, "message", itemID, ts, mark, dateDue)
	if err != nil {
		h.logger.Error("SavedUpdate failed",
			zap.String("item_id", itemID),
			zap.String("ts", ts),
			zap.String("mark", mark),
			zap.Int64("date_due", dateDue),
			zap.Error(err))
		return nil, fmt.Errorf("failed to update saved item: %v", err)
	}

	action := "updated"
	if mark == "completed" {
		action = "marked as completed"
	}
	if dateDue > 0 {
		dueTime := time.Unix(dateDue, 0).UTC().Format("2006-01-02 15:04")
		action += fmt.Sprintf(", due date set to %s", dueTime)
	}

	return mcp.NewToolResultText(fmt.Sprintf("Successfully %s saved item (item_id=%s, ts=%s)", action, itemID, ts)), nil
}

func (h *SavedHandler) SavedClearCompletedHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	h.logger.Debug("SavedClearCompletedHandler called", zap.Any("params", request.Params))

	err := h.apiProvider.Slack().SavedClearCompleted(ctx)
	if err != nil {
		h.logger.Error("SavedClearCompleted failed", zap.Error(err))
		return nil, fmt.Errorf("failed to clear completed saved items: %v", err)
	}

	return mcp.NewToolResultText("Successfully cleared all completed saved items"), nil
}

func (h *SavedHandler) SavedRemoveHandler(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	h.logger.Debug("SavedRemoveHandler called", zap.Any("params", request.Params))

	itemID := request.GetString("item_id", "")
	ts := request.GetString("ts", "")
	if itemID == "" || ts == "" {
		return nil, fmt.Errorf("item_id and ts are required parameters")
	}

	if err := h.apiProvider.Slack().SavedDelete(ctx, "message", itemID, ts); err != nil {
		h.logger.Error("SavedDelete failed",
			zap.String("item_id", itemID),
			zap.String("ts", ts),
			zap.Error(err))
		return nil, fmt.Errorf("failed to remove saved item: %v", err)
	}

	return mcp.NewToolResultText(fmt.Sprintf("Successfully removed saved item (item_id=%s, ts=%s)", itemID, ts)), nil
}

// savedPermalink builds a message link from the workspace URL, e.g. https://x.slack.com/.
func savedPermalink(workspaceURL, channelID, ts, threadTs string) string {
	if workspaceURL == "" || channelID == "" || ts == "" {
		return ""
	}
	link := fmt.Sprintf("%sarchives/%s/p%s", strings.TrimSuffix(workspaceURL, "/")+"/", channelID, strings.ReplaceAll(ts, ".", ""))
	if threadTs != "" && threadTs != ts {
		link += fmt.Sprintf("?thread_ts=%s&cid=%s", threadTs, channelID)
	}
	return link
}

func formatUnixTs(ts int64) string {
	if ts == 0 {
		return ""
	}
	return time.Unix(ts, 0).UTC().Format("2006-01-02 15:04")
}

// fetchSavedItemMessages returns the saved message, or its thread replies up to it.
func (h *SavedHandler) fetchSavedItemMessages(ctx context.Context, item edge.SavedItem, channelName string, maxMsgsPerItem int) []Message {
	histParams := &slack.GetConversationHistoryParameters{
		ChannelID: item.ItemID,
		Latest:    item.Ts,
		Oldest:    item.Ts,
		Inclusive: true,
		Limit:     1,
	}
	histResp, err := h.apiProvider.Slack().GetConversationHistoryContext(ctx, histParams)
	if err != nil {
		h.logger.Warn("Failed to fetch saved message via history, trying replies",
			zap.String("channel", item.ItemID),
			zap.String("ts", item.Ts),
			zap.Error(err))
		return []Message{{
			MsgID:   item.Ts,
			Channel: channelName,
			Text:    "[message content unavailable — channel access denied]",
			Time:    formatUnixTs(item.DateCreated),
		}}
	}
	if len(histResp.Messages) > 0 {
		msg := histResp.Messages[0]
		if msg.ThreadTimestamp != "" && msg.ThreadTimestamp != msg.Timestamp {
			repliesParams := &slack.GetConversationRepliesParameters{
				ChannelID: item.ItemID,
				Timestamp: msg.ThreadTimestamp,
				Latest:    item.Ts,
				Oldest:    item.Ts,
				Inclusive: true,
				Limit:     maxMsgsPerItem,
			}
			replies, _, _, err := h.apiProvider.Slack().GetConversationRepliesContext(ctx, repliesParams)
			if err == nil && len(replies) > 0 {
				msgs := h.convHandler.convertMessagesFromHistory(ctx, replies, item.ItemID, false)
				return msgs
			}
		}
		msgs := h.convHandler.convertMessagesFromHistory(ctx, histResp.Messages, item.ItemID, false)
		return msgs
	} else {
		repliesParams := &slack.GetConversationRepliesParameters{
			ChannelID: item.ItemID,
			Timestamp: item.Ts,
			Latest:    item.Ts,
			Oldest:    item.Ts,
			Inclusive: true,
			Limit:     maxMsgsPerItem,
		}
		replies, _, _, err := h.apiProvider.Slack().GetConversationRepliesContext(ctx, repliesParams)
		if err == nil && len(replies) > 0 {
			msgs := h.convHandler.convertMessagesFromHistory(ctx, replies, item.ItemID, false)
			return msgs
		} else {
			return []Message{{
				MsgID:   item.Ts,
				Channel: channelName,
				Text:    "[saved item — message not found in channel history]",
				Time:    formatUnixTs(item.DateCreated),
			}}
		}
	}
}
