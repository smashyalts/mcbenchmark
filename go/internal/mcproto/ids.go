package mcproto

// Packet IDs for protocol 775 (Minecraft 26.1.2), extracted from the server's
// own data generator (`--reports` → generated/reports/packets.json). That is
// the authoritative source; regenerate it when retargeting another version and
// update this file. Login and configuration IDs have been stable for several
// versions; the play phase IDs shift frequently, so treat them as version-bound.

const ProtocolDefault = 775

// Handshaking, serverbound.
const (
	SBHandshake int32 = 0x00 // intention
)

// Handshake intents.
const (
	IntentStatus   int32 = 1
	IntentLogin    int32 = 2
	IntentTransfer int32 = 3
)

// Login, clientbound.
const (
	CBLoginDisconnect        int32 = 0x00 // login_disconnect
	CBLoginEncryptionRequest int32 = 0x01 // hello
	CBLoginSuccess           int32 = 0x02 // login_finished
	CBLoginSetCompression    int32 = 0x03 // login_compression
	CBLoginPluginRequest     int32 = 0x04 // custom_query
	CBLoginCookieRequest     int32 = 0x05 // cookie_request
)

// Login, serverbound.
const (
	SBLoginStart          int32 = 0x00 // hello
	SBLoginPluginResponse int32 = 0x02 // custom_query_answer
	SBLoginAcknowledged   int32 = 0x03 // login_acknowledged
)

// Configuration, clientbound.
const (
	CBConfigCookieRequest int32 = 0x00
	CBConfigPluginMessage int32 = 0x01 // custom_payload
	CBConfigDisconnect    int32 = 0x02
	CBConfigFinish        int32 = 0x03 // finish_configuration
	CBConfigKeepAlive     int32 = 0x04
	CBConfigPing          int32 = 0x05
	CBConfigRegistryData  int32 = 0x07
	CBConfigKnownPacks    int32 = 0x0E // select_known_packs
	CBConfigCodeOfConduct int32 = 0x13 // code_of_conduct (26.x); requires accept before finish
)

// Configuration, serverbound.
const (
	SBConfigClientInformation int32 = 0x00 // client_information
	SBConfigFinishAck         int32 = 0x03 // finish_configuration
	SBConfigKeepAlive         int32 = 0x04
	SBConfigPong              int32 = 0x05
	SBConfigKnownPacks        int32 = 0x07 // select_known_packs
	SBConfigAcceptCoC         int32 = 0x09 // accept_code_of_conduct
)

// Play, clientbound (only the packets the replay client reacts to).
const (
	CBPlayContainerClose      int32 = 0x11 // container_close
	CBPlayContainerSetContent int32 = 0x12 // container_set_content (carries state_id)
	CBPlayContainerSetSlot    int32 = 0x14 // container_set_slot (carries state_id)
	CBPlayBlockUpdate         int32 = 0x08 // block_update (server's verdict on a dig)
	CBPlayDisconnect          int32 = 0x20
	CBPlayChunkBatchFinished  int32 = 0x0B // chunk_batch_finished
	CBPlayKeepAlive           int32 = 0x2C
	CBPlayOpenScreen          int32 = 0x3B // open_screen (carries live window id)
	CBPlayPing                int32 = 0x3D
	CBPlaySyncPosition        int32 = 0x48 // player_position
	CBPlayStartConfiguration  int32 = 0x76 // start_configuration
)

// Play, serverbound.
const (
	SBPlayTeleportConfirm    int32 = 0x00 // accept_teleportation
	SBPlayChatCommand        int32 = 0x07 // chat_command
	SBPlayChunkBatchReceived int32 = 0x0B // chunk_batch_received
	SBPlayTickEnd            int32 = 0x0D // client_tick_end
	SBPlayConfigurationAck   int32 = 0x10 // configuration_acknowledged
	SBPlayContainerClick     int32 = 0x12 // container_click
	SBPlayContainerClose     int32 = 0x13 // container_close
	SBPlayKeepAlive          int32 = 0x1C
	SBPlayPosition           int32 = 0x1E // move_player_pos
	SBPlayPositionLook       int32 = 0x1F // move_player_pos_rot
	SBPlayLook               int32 = 0x20 // move_player_rot
	SBPlayFlying             int32 = 0x21 // move_player_status_only
	SBPlayBlockDig           int32 = 0x29 // player_action
	SBPlayEntityAction       int32 = 0x2A // player_command
	SBPlayPlayerLoaded       int32 = 0x2C // player_loaded
	SBPlayPong               int32 = 0x2D
	SBPlayAbilities          int32 = 0x28 // player_abilities (toggle flight)
	SBPlaySetCreativeSlot    int32 = 0x38 // set_creative_mode_slot (creative only)
	SBPlayArmAnimation       int32 = 0x3F // swing
	SBPlayBlockPlace         int32 = 0x42 // use_item_on
	SBPlayUseItem            int32 = 0x43 // use_item
	SBPlaySetCarriedItem     int32 = 0x35 // set_carried_item (hotbar slot)
	SBPlayChat               int32 = 0x09 // chat
	SBPlayAttack             int32 = 0x01 // attack (26.x split this out of interact)
	SBPlayInteract           int32 = 0x1A // interact (right-click an entity)
)

// Clientbound packets the replay client reads to track the live world.
const (
	CBPlayAddEntity           int32 = 0x01 // add_entity
	CBPlayRemoveEntities      int32 = 0x4D // remove_entities
	CBPlayLevelChunk          int32 = 0x2D // level_chunk_with_light
	CBPlaySectionBlocksUpdate int32 = 0x54 // section_blocks_update
)

// Block-dig statuses (serverbound player_action). The server runs a small state
// machine across them: it only accepts a "stop" for the position it previously
// saw a "start" for.
//
// The list continues past digging: the same packet carries dropping items and
// swapping hands, which is why they are replayed through it rather than through
// an inventory click.
const (
	DigStart      int32 = 0
	DigAbort      int32 = 1
	DigFinish     int32 = 2
	DropItemStack int32 = 3 // drop the whole stack (ctrl-Q)
	DropItem      int32 = 4 // drop one item (Q)
	SwapHands     int32 = 6 // offhand swap (F)
)

// Entity action IDs (serverbound player_command).
const (
	ActionStartSneak  int32 = 0
	ActionStopSneak   int32 = 1
	ActionStartSprint int32 = 3
	ActionStopSprint  int32 = 4
)
