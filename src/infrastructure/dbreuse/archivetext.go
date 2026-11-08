package dbreuse

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// archiveMessageText returns text GOWA would store for a history-sync message.
// It reads supported protobuf JSON fields only. Quoted messages, reaction
// text, media keys, and opaque button-parameter JSON are not turned into text.
func archiveMessageText(message map[string]json.RawMessage) (string, bool) {
	return messageText(message, 0)
}

func messageText(message map[string]json.RawMessage, depth int) (string, bool) {
	if message == nil || depth > 4 {
		return "", false
	}
	message = unwrapArchiveMessage(message, depth)
	if text, ok := jsonString(message, "conversation"); ok && text != "" {
		return text, true
	}
	if extended := objectField(message, "extendedTextMessage"); extended != nil {
		if text, ok := jsonString(extended, "text"); ok && text != "" {
			return text, true
		}
		if text, ok := jsonString(extended, "matchedText"); ok && text != "" {
			return text, true
		}
	}
	for _, media := range []string{"imageMessage", "videoMessage", "documentMessage", "ptvMessage"} {
		if obj := objectField(message, media); obj != nil {
			if text, ok := jsonString(obj, "caption"); ok && text != "" {
				return text, true
			}
		}
	}
	if text, ok := templateText(objectField(message, "templateMessage")); ok {
		return text, true
	}
	if text, ok := interactiveText(objectField(message, "interactiveMessage")); ok {
		return text, true
	}
	if buttons := objectField(message, "buttonsMessage"); buttons != nil {
		if text := joinNonEmpty(jsonStringValue(buttons, "contentText"), jsonStringValue(buttons, "footerText"), buttonDisplayTexts(buttons)); text != "" {
			return text, true
		}
	}
	if reply := objectField(message, "buttonsResponseMessage"); reply != nil {
		if text := joinNonEmpty(jsonStringValue(reply, "selectedDisplayText"), responseDisplayText(reply)); text != "" {
			return text, true
		}
	}
	if reply := objectField(message, "templateButtonReplyMessage"); reply != nil {
		if text, ok := jsonString(reply, "selectedDisplayText"); ok && text != "" {
			return text, true
		}
	}
	if list := objectField(message, "listMessage"); list != nil {
		if text := joinNonEmpty(jsonStringValue(list, "title"), jsonStringValue(list, "description"), jsonStringValue(list, "buttonText")); text != "" {
			return text, true
		}
	}
	if list := objectField(message, "listResponseMessage"); list != nil {
		if text := joinNonEmpty(jsonStringValue(list, "title"), jsonStringValue(list, "description")); text != "" {
			return text, true
		}
	}
	if text, ok := contactText(message); ok {
		return text, true
	}
	if text, ok := locationText(message); ok {
		return text, true
	}
	for _, poll := range []string{"pollCreationMessage", "pollCreationMessageV2", "pollCreationMessageV3", "pollCreationMessageV4", "pollCreationMessageV5", "pollCreationMessageV6"} {
		if obj := objectField(message, poll); obj != nil {
			if text, ok := jsonString(obj, "name"); ok && text != "" {
				return text, true
			}
		}
	}
	if product := objectField(message, "productMessage"); product != nil {
		item := objectField(product, "product")
		if text := joinNonEmpty(jsonStringValue(item, "title"), jsonStringValue(item, "description")); text != "" {
			return text, true
		}
	}
	if rich := objectField(message, "richResponseMessage"); rich != nil {
		if text := richMessageText(rich); text != "" {
			return text, true
		}
	}
	if depth == 0 {
		if payment := objectField(message, "sendPaymentMessage"); payment != nil {
			if note := objectField(payment, "noteMessage"); note != nil {
				if text, ok := messageText(note, depth+1); ok {
					return text, true
				}
			}
		}
		if protocol := objectField(message, "protocolMessage"); protocol != nil {
			if edited := objectField(protocol, "editedMessage"); edited != nil {
				if text, ok := messageText(edited, depth+1); ok {
					return text, true
				}
			}
		}
	}
	return "", false
}

func unwrapArchiveMessage(message map[string]json.RawMessage, depth int) map[string]json.RawMessage {
	if message == nil || depth > 4 {
		return message
	}
	// Order matches UnwrapMessage, then the inner-message carriers present in
	// history JSON. Each step follows one wrapper so a caption is not skipped.
	wrappers := []string{
		"viewOnceMessage",
		"ephemeralMessage",
		"viewOnceMessageV2",
		"viewOnceMessageV2Extension",
		"documentWithCaptionMessage",
		"associatedChildMessage",
		"commentMessage",
		"deviceSentMessage",
		"groupStatusMentionMessage",
	}
	for _, name := range wrappers {
		wrap := objectField(message, name)
		if wrap == nil {
			continue
		}
		inner := objectField(wrap, "message")
		if inner == nil {
			continue
		}
		return unwrapArchiveMessage(inner, depth+1)
	}
	return message
}

func templateText(template map[string]json.RawMessage) (string, bool) {
	if template == nil {
		return "", false
	}
	format := objectField(template, "Format")
	if format == nil {
		format = template
	}
	if hydrated := objectField(format, "HydratedFourRowTemplate"); hydrated != nil {
		title := ""
		if titleObj := objectField(hydrated, "Title"); titleObj != nil {
			title = jsonStringValue(titleObj, "HydratedTitleText")
		}
		text := joinNonEmpty(title, jsonStringValue(hydrated, "hydratedContentText"), jsonStringValue(hydrated, "hydratedFooterText"), hydratedButtonTexts(hydrated))
		if text != "" {
			return text, true
		}
	}
	if interactive := objectField(format, "InteractiveMessageTemplate"); interactive != nil {
		text := joinNonEmpty(jsonStringValue(objectField(interactive, "body"), "text"), jsonStringValue(objectField(interactive, "footer"), "text"), carouselText(interactive))
		if text != "" {
			return text, true
		}
	}
	return "", false
}

func interactiveText(interactive map[string]json.RawMessage) (string, bool) {
	if interactive == nil {
		return "", false
	}
	text := joinNonEmpty(jsonStringValue(objectField(interactive, "body"), "text"), jsonStringValue(objectField(interactive, "footer"), "text"), carouselText(interactive))
	if text != "" {
		return text, true
	}
	if inner := objectField(interactive, "InteractiveMessage"); inner != nil {
		return interactiveText(inner)
	}
	return "", false
}

func carouselText(interactive map[string]json.RawMessage) string {
	if interactive == nil {
		return ""
	}
	flow := objectField(interactive, "InteractiveMessage")
	if flow == nil {
		flow = interactive
	}
	carousel := objectField(flow, "CarouselMessage")
	if carousel == nil {
		return ""
	}
	raw, ok := carousel["cards"]
	if !ok {
		return ""
	}
	var cards []map[string]json.RawMessage
	if json.Unmarshal(raw, &cards) != nil {
		return ""
	}
	var lines []string
	for _, card := range cards {
		body := objectField(card, "body")
		if body == nil {
			if inner := objectField(card, "InteractiveMessage"); inner != nil {
				body = objectField(inner, "body")
			}
		}
		if text, ok := jsonString(body, "text"); ok && text != "" {
			lines = append(lines, text)
		}
	}
	return strings.Join(lines, "\n")
}

func hydratedButtonTexts(hydrated map[string]json.RawMessage) string {
	raw, ok := hydrated["hydratedButtons"]
	if !ok {
		return ""
	}
	var buttons []map[string]json.RawMessage
	if json.Unmarshal(raw, &buttons) != nil {
		return ""
	}
	var lines []string
	for _, button := range buttons {
		current := objectField(button, "HydratedButton")
		if current == nil {
			current = button
		}
		for _, kind := range []string{"UrlButton", "QuickReplyButton", "CallButton"} {
			if text, ok := jsonString(objectField(current, kind), "displayText"); ok && text != "" {
				lines = append(lines, text)
			}
		}
	}
	return strings.Join(lines, "\n")
}

func buttonDisplayTexts(buttons map[string]json.RawMessage) string {
	raw, ok := buttons["buttons"]
	if !ok {
		return ""
	}
	var items []map[string]json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return ""
	}
	var lines []string
	for _, item := range items {
		textObj := objectField(item, "buttonText")
		if text, ok := jsonString(textObj, "displayText"); ok && text != "" {
			lines = append(lines, text)
		}
	}
	return strings.Join(lines, "\n")
}

func responseDisplayText(reply map[string]json.RawMessage) string {
	response := objectField(reply, "Response")
	if response == nil {
		return ""
	}
	return jsonStringValue(response, "SelectedDisplayText")
}

func contactText(message map[string]json.RawMessage) (string, bool) {
	if contact := objectField(message, "contactMessage"); contact != nil {
		text := formatContactSummary(jsonStringValue(contact, "displayName"), firstVCardTel(jsonStringValue(contact, "vcard")), false)
		if text != "" {
			return text, true
		}
	}
	if contacts := objectField(message, "contactsArrayMessage"); contacts != nil {
		name := jsonStringValue(contacts, "displayName")
		phone := ""
		if raw, ok := contacts["contacts"]; ok {
			var items []map[string]json.RawMessage
			if json.Unmarshal(raw, &items) == nil && len(items) > 0 {
				if name == "" {
					name = jsonStringValue(items[0], "displayName")
				}
				phone = firstVCardTel(jsonStringValue(items[0], "vcard"))
			}
		}
		text := formatContactSummary(name, phone, true)
		if text != "" {
			return text, true
		}
	}
	return "", false
}

func locationText(message map[string]json.RawMessage) (string, bool) {
	for _, name := range []string{"locationMessage", "liveLocationMessage"} {
		loc := objectField(message, name)
		if loc == nil {
			continue
		}
		lat, latOK := jsonFloat(loc, "degreesLatitude")
		long, longOK := jsonFloat(loc, "degreesLongitude")
		if !latOK || !longOK {
			continue
		}
		label := jsonStringValue(loc, "name")
		if label == "" {
			label = jsonStringValue(loc, "caption")
		}
		return formatLocationSummary(label, jsonStringValue(loc, "address"), lat, long), true
	}
	return "", false
}

func richMessageText(rich map[string]json.RawMessage) string {
	raw, ok := rich["submessages"]
	if !ok {
		return ""
	}
	var items []map[string]json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return ""
	}
	var lines []string
	for _, item := range items {
		if text, ok := jsonString(item, "messageText"); ok && text != "" {
			lines = append(lines, text)
		}
	}
	return strings.Join(lines, "\n")
}

func archiveTextShape(message map[string]json.RawMessage) string {
	message = unwrapArchiveMessage(message, 0)
	if message == nil {
		return "none"
	}
	var keys []string
	for key := range message {
		if key == "messageContextInfo" {
			continue
		}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return "none"
	}
	sort.Strings(keys)
	return strings.Join(keys, "+")
}

func senderShape(fromMe, keyParticipant, infoParticipant bool, chat string) string {
	return fmt.Sprintf("from_me=%t;key_participant=%s;info_participant=%s;chat=%s", fromMe, presence(keyParticipant), presence(infoParticipant), chatServer(chat))
}

func presence(value bool) string {
	if value {
		return "present"
	}
	return "absent"
}

func chatServer(chat string) string {
	at := strings.LastIndex(chat, "@")
	if at < 0 || at == len(chat)-1 {
		return "unknown"
	}
	switch chat[at+1:] {
	case "s.whatsapp.net", "g.us", "broadcast", "newsletter", "lid":
		return chat[at+1:]
	default:
		return "other"
	}
}

func directChat(chat string) bool {
	server := chatServer(chat)
	return server == "s.whatsapp.net" || server == "lid"
}

func objectField(message map[string]json.RawMessage, key string) map[string]json.RawMessage {
	if message == nil {
		return nil
	}
	raw, ok := message[key]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	return obj
}

func jsonString(message map[string]json.RawMessage, key string) (string, bool) {
	if message == nil {
		return "", false
	}
	raw, ok := message[key]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return "", false
	}
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return "", false
	}
	return text, true
}

func jsonStringValue(message map[string]json.RawMessage, key string) string {
	text, _ := jsonString(message, key)
	return text
}

func jsonFloat(message map[string]json.RawMessage, key string) (float64, bool) {
	if message == nil {
		return 0, false
	}
	raw, ok := message[key]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return 0, false
	}
	var number float64
	if json.Unmarshal(raw, &number) == nil {
		return number, true
	}
	text := strings.Trim(string(raw), `"`)
	number, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0, false
	}
	return number, true
}

func joinNonEmpty(parts ...string) string {
	var lines []string
	for _, part := range parts {
		if part != "" {
			lines = append(lines, part)
		}
	}
	return strings.Join(lines, "\n")
}

func formatContactSummary(name, phone string, plural bool) string {
	name = strings.TrimSpace(name)
	phone = strings.TrimSpace(phone)
	prefix := "Contact"
	if plural {
		prefix = "Contacts"
	}
	switch {
	case name != "" && phone != "":
		return prefix + ": " + name + " (" + phone + ")"
	case name != "":
		return prefix + ": " + name
	case phone != "":
		return prefix + ": " + phone
	default:
		return ""
	}
}

func formatLocationSummary(name, address string, lat, long float64) string {
	var parts []string
	if name = strings.TrimSpace(name); name != "" {
		parts = append(parts, name)
	}
	if address = strings.TrimSpace(address); address != "" {
		parts = append(parts, address)
	}
	parts = append(parts, fmt.Sprintf("https://maps.google.com/?q=%g,%g", lat, long))
	return strings.Join(parts, " — ")
}

func firstVCardTel(vcard string) string {
	if vcard == "" {
		return ""
	}
	normalized := strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(vcard)
	for _, line := range strings.Split(normalized, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name := line
		if idx := strings.IndexAny(name, ";:"); idx >= 0 {
			name = name[:idx]
		}
		if idx := strings.LastIndex(name, "."); idx >= 0 {
			name = name[idx+1:]
		}
		if !strings.EqualFold(strings.TrimSpace(name), "TEL") {
			continue
		}
		if idx := strings.LastIndex(line, ":"); idx >= 0 {
			return strings.TrimSpace(line[idx+1:])
		}
	}
	return ""
}
