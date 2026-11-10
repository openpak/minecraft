package walk

// layouts holds the read() of every packet id in the Wii U v688 id table (the 129 ids shared with PS3 01.84 plus A2). The
// comment on each entry is its Wii U read() address; the field order is that function's. Both directions share one id
// space, so one table serves host->joiner and joiner->host.
var layouts = map[byte]func(*rd){
	0x00: func(r *rd) { r.skip(4) },                                                // ClientboundKeepAlive: int
	0x01: login,                                                                    // ClientboundLogin 021db3c4 (sent both ways)
	0x02: clientPreLogin,                                                           // ClientboundPreLogin 021dcfac
	0x03: func(r *rd) { r.skip(2); r.flexible() },                                  // ClientboundChat: short + FlexiblePacket
	0x04: func(r *rd) { r.skip(16) },                                               // SetTime: long, long
	0x05: func(r *rd) { r.varint(); r.skip(2); r.item() },                          // SetEquippedItem 02220244
	0x06: func(r *rd) { r.skip(8) },                                                // SetSpawnPosition: block pos
	0x07: func(r *rd) { r.varint(); r.skip(4 + 12 + 1) },                           // ServerboundInteract: VarInt, int, 3 floats, byte
	0x08: func(r *rd) { r.skip(4); r.varint(); r.skip(5); r.varint(); r.varint() }, // SetHealth: float, VarInt, float, byte, 2 VarInt
	0x09: respawn,                                                                  // Respawn 021de740
	0x0A: func(r *rd) { r.skip(1) },                                                // MovePlayer: flags
	0x0B: func(r *rd) { r.skip(24 + 1) },                                           // MovePlayer::Pos 028ad7b0
	0x0C: func(r *rd) { r.skip(8 + 1) },                                            // MovePlayer::Rot 028ada24
	0x0D: func(r *rd) { r.skip(24 + 8 + 1) },                                       // MovePlayer::PosRot 028ad4c0
	0x0E: func(r *rd) { r.skip(4 + 8 + 1 + 4) },                                    // PlayerAction: int, block pos, ubyte, int
	0x0F: func(r *rd) { r.skip(1) },                                                // UseItem
	0x10: func(r *rd) { r.skip(1) },                                                // SetCarriedItem
	0x11: func(r *rd) { r.varint(); r.skip(8) },                                    // PlayerSleep
	0x12: func(r *rd) { r.skip(5) },                                                // Animate: int, byte
	0x13: func(r *rd) { r.varint(); r.skip(4); r.varint() },                        // PlayerCommand
	0x14: addPlayer,                                                                // AddPlayer 021d1a2c
	0x16: func(r *rd) { r.varint(); r.varint(); r.varint() },                       // TakeItemEntity
	0x17: addEntity,                                                                // AddEntity 021cdd88
	0x18: addMob,                                                                   // AddMob 021cfd14
	0x19: func(r *rd) { r.skip(2); r.str(); r.skip(8 + 1) },                        // AddPainting: ushort, utf, block pos, ubyte
	0x1A: func(r *rd) { r.skip(16 + 2) },                                           // AddExperienceOrb
	0x1B: func(r *rd) { r.skip(9) },                                                // PlayerInput: 2 floats, byte
	0x1C: func(r *rd) { // SetEntityMotion 0221e32c: short id|flags; 0x800: three bytes, else three shorts
		if r.u16()&0x800 != 0 {
			r.skip(3)
		} else {
			r.skip(6)
		}
	},
	0x1D: func(r *rd) { r.skip(4 * r.varint()) },                                 // RemoveEntities 021ddd70
	0x1E: func(r *rd) { r.skip(2) },                                              // MoveEntity 021dbff0
	0x1F: func(r *rd) { r.skip(2 + 4) },                                          // MoveEntity::Pos 021dc81c: packed int
	0x20: func(r *rd) { r.skip(2 + 3) },                                          // MoveEntity::Rot 021dcaec: byte, byte, bool
	0x21: func(r *rd) { r.skip(2 + 4 + 2) },                                      // MoveEntity::PosRot 021dc4ec
	0x22: func(r *rd) { r.varint(); r.skip(6 + 2 + 1) },                          // TeleportEntity
	0x23: func(r *rd) { r.varint(); r.skip(1) },                                  // RotateHead
	0x26: func(r *rd) { r.skip(9) },                                              // EntityEvent: int, byte, int
	0x27: func(r *rd) { r.skip(8) },                                              // SetEntityLink
	0x28: func(r *rd) { r.varint(); r.skip(4); r.meta() },                        // SetEntityData 0221d5c0
	0x29: func(r *rd) { r.varint(); r.skip(2); r.varint(); r.skip(1) },           // UpdateMobEffect
	0x2A: func(r *rd) { r.varint(); r.skip(1) },                                  // RemoveMobEffect
	0x2B: func(r *rd) { r.skip(4); r.varint(); r.varint() },                      // SetExperience
	0x2C: updateAttributes,                                                       // UpdateAttributes 0222c1d0
	0x32: func(r *rd) { r.skip(4) },                                              // ChunkVisibility
	0x33: blockRegion,                                                            // BlockRegionUpdate 02189018
	0x34: chunkBlocks,                                                            // ChunkBlocksUpdate 021d34fc
	0x35: func(r *rd) { r.skip(8) },                                              // BlockUpdate 021df548: int, short, short
	0x36: func(r *rd) { r.skip(8 + 2 + 2) },                                      // BlockEvent: block pos, ubyte, ubyte, short
	0x37: func(r *rd) { r.skip(4 + 8 + 1) },                                      // BlockDestruction
	0x3C: explode,                                                                // Explode 021d9674
	0x3D: func(r *rd) { r.skip(4 + 8 + 4 + 1) },                                  // LevelEvent
	0x3E: func(r *rd) { r.varint(); r.varint(); r.skip(12 + 8 + 1 + 4 + 1 + 4) }, // Sound 02229594
	0x3F: particles,                                                              // LevelParticles 021e2bcc
	0x46: func(r *rd) { r.skip(9) },                                              // GameEvent: ubyte, float, int
	0x47: func(r *rd) { r.skip(4 + 1 + 12) },                                     // AddGlobalEntity
	0x64: containerOpen,                                                          // ContainerOpen 021d542c
	0x65: func(r *rd) { r.skip(1) },                                              // ContainerClose
	0x66: func(r *rd) { r.skip(7); items(r, r.u8()) },                            // ServerboundContainerClick 028ab060
	0x67: func(r *rd) { r.skip(3); r.item() },                                    // ContainerSetSlot 021d795c
	0x68: func(r *rd) { r.skip(1); items(r, r.i16()) },                           // ContainerSetContent 021d6280
	0x69: func(r *rd) { r.skip(5) },                                              // ContainerSetData
	0x6A: func(r *rd) { r.skip(4) },                                              // ContainerAck: byte, short, byte
	0x6B: func(r *rd) { r.skip(2); r.item() },                                    // SetCreativeModeSlot 028b0fb0
	0x6C: func(r *rd) { r.skip(2) },                                              // ContainerButtonClick
	0x84: func(r *rd) { r.skip(8 + 2); r.cnbt() },                                // BlockEntityData
	0x85: func(r *rd) { r.skip(24 + 8) },                                         // MoveVehicle
	0x86: func(r *rd) {
		r.varint()
		n := r.varint()
		for k := 0; k < n && r.err == nil; k++ {
			r.varint()
		}
	}, // SetPassengers
	0x87: func(r *rd) { r.varint() },                                             // AcceptTeleportation
	0x88: func(r *rd) { r.skip(24 + 8) },                                         // ServerboundMoveVehicle
	0x89: func(r *rd) { r.skip(8 + 2 + 12) },                                     // UseItemOn
	0x8A: func(r *rd) { r.skip(2) },                                              // PaddleBoat
	0x8B: bossEvent,                                                              // BossEvent 021fd7cc
	0x96: craftItem,                                                              // CraftItem 0222e27c
	0x97: func(r *rd) { r.skip(8) },                                              // TradeItem
	0x98: func(r *rd) { r.skip(4) },                                              // DebugOptions
	0x99: func(r *rd) { r.skip(5) },                                              // ServerSettingsChanged
	0x9A: func(r *rd) { r.str(); r.skip(r.u16()) },                               // Texture 0299fe10
	0x9B: func(r *rd) { r.skip(8) },                                              // ChunkVisibilityArea
	0x9C: func(r *rd) { r.skip(1) },                                              // UpdateProgress
	0x9D: func(r *rd) { r.skip(5); r.str() },                                     // TextureChange: int, byte, utf
	0x9E: func(r *rd) { r.skip(4); r.str(); r.skip(9); r.skip(max(r.i32(), 0)) }, // UpdateGameRuleProgress 02a6231c
	0x9F: func(r *rd) { r.skip(1) },                                              // KickPlayer
	0xA0: textureGeometry,                                                        // TextureAndGeometry 029a1468
	0xA1: func(r *rd) { r.skip(8); r.str() },                                     // TextureAndGeometryChange
	0xA2: func(r *rd) { r.skip(2) },                                              // MoveEntityPacketSmall (base, Wii U table only)
	0xA3: func(r *rd) { r.skip(4) },                                              // MoveEntityPacketSmall::Pos 02673ae8: packed int
	0xA4: func(r *rd) { r.skip(3) },                                              // ::Rot 02673de0: short, byte
	0xA5: func(r *rd) { r.skip(6) },                                              // ::PosRot 02673734: int, short
	0xA6: func(r *rd) { r.skip(10) },                                             // XZ: char, int, int
	0xA7: func(r *rd) { r.skip(4); r.shortBytes() },                              // GameCommand 02421b6c
	0xA8: func(r *rd) { r.shortBytes() },                                         // MapSelectInfo 025e045c
	0xA9: func(r *rd) { r.skip(13) },                                             // Vote: int, byte, int, int
	0xAA: func(r *rd) { r.skip(5) },                                              // PlayerReady
	0xAB: func(r *rd) { r.skip(8) },                                              // Powerup
	0xC8: func(r *rd) { r.skip(4); r.skip(max(r.i32(), 0)) },                     // AwardStat 021d2b8c
	0xC9: func(r *rd) { r.skip(12) },                                             // PlayerInfo: byte, short, int, int, byte
	0xCA: func(r *rd) { r.skip(13) },                                             // ClientboundPlayerAbilities: byte, 2 floats, int
	0xCD: func(r *rd) { r.skip(4) },                                              // ClientCommand
	0xCE: scoreboard,                                                             // Scoreboard 028a1bcc
	0xD1: playerTeam,                                                             // SetPlayerTeam 02223c10
	0xD2: func(r *rd) { r.skip(1); r.flexible() },                                // GameMode: byte + FlexiblePacket
	0xD3: mapItem,                                                                // MapItemData 021e4698
	0xD4: func(r *rd) { r.skip(8) },                                              // OpenSignEditor
	0xD5: combat,                                                                 // PlayerCombat 021e82a8
	0xD6: func(r *rd) {
		r.skip(24)
		if r.u8() != 0 {
			r.skip(8)
		}
		r.varint()
	}, // PlayerPosition 021ccd1c
	0xD8: func(r *rd) { r.str(); r.str() }, // ResourcePack
	0xD9: setBorder,                        // SetBorder 021e9588
	0xDA: func(r *rd) { r.varint() },       // SetCamera
	0xDB: func(r *rd) { // SetTitles 02224eac
		switch a := r.u32(); {
		case a < 3:
			r.str()
		case a == 3:
			r.skip(12)
		}
	},
	0xDC: func(r *rd) { r.str(); r.str() }, // TabList
	0xDD: func(r *rd) {
		r.str()
		r.skip(1)
		if r.u8() != 0 {
			r.skip(8)
		}
	}, // ChatAutoComplete 028a9374
	0xDE: func(r *rd) { r.str() },                                       // ServerboundChat
	0xDF: func(r *rd) { r.skip(4) },                                     // ServerboundContainerAck: byte, short, bool
	0xE0: func(r *rd) { r.skip(1) },                                     // ServerboundContainerClose
	0xE1: func(r *rd) { r.str(); r.shortBytes() },                       // ServerboundCustomPayload 028abcd0
	0xE2: func(r *rd) { r.varint() },                                    // ServerboundKeepAlive
	0xE3: func(r *rd) { r.skip(9) },                                     // ServerboundPlayerAbilities
	0xE4: func(r *rd) { r.skip(4) },                                     // ServerboundResourcePack
	0xE5: func(r *rd) { r.skip(2) },                                     // ServerboundSetCarriedItem
	0xE6: func(r *rd) { r.skip(9); r.str(); r.str(); r.str(); r.str() }, // SignUpdate 028b168c
	0xE7: func(r *rd) { r.uid() },                                       // TeleportToEntity
	0xE8: func(r *rd) { r.ver = r.i; r.skip(2); r.str() },               // ServerboundPreLogin
	0xE9: func(r *rd) { r.skip(1) },                                     // Swing 028b1d2c: read() of one byte
	0xEA: func(r *rd) { r.skip(9) },                                     // DamageIndicator
	0xEB: func(r *rd) { r.skip(5); r.skip(8 * r.u16()) },                // BlockCollectionDestruction 021e1e6c
	0xEC: func(r *rd) { r.varint(); r.varint() },                        // Cooldown
	0xEF: mgSettings,                                                    // MGPlayerSettingsUpdate 021eb4e8
	0xFA: func(r *rd) { r.str(); r.shortBytes() },                       // ClientboundCustomPayload 021e0604
	0xFD: func(r *rd) { r.skip(1) },                                     // ChangeDifficulty
	0xFE: func(r *rd) {},                                                // GetInfo
	0xFF: func(r *rd) { r.skip(4) },                                     // Disconnect
}

func items(r *rd, n int) {
	for k := 0; k < n && r.err == nil; k++ {
		r.item()
	}
}

func login(r *rd) {
	r.skip(4)
	r.str()
	r.str()
	r.skip(8 + 12 + 3)
	r.uid()
	r.uid()
	r.skip(1 + 4 + 1 + 4 + 1 + 4 + 4 + 2 + 8 + 2 + 1 + 12)
}

func clientPreLogin(r *rd) {
	r.ver = r.i
	r.skip(2)
	r.str()
	r.skip(5)
	n := r.u8()
	for k := 0; k < n && r.err == nil; k++ {
		r.uid()
	}
	r.skip(14 + 4 + 1 + 4 + 1 + 4)
}

func respawn(r *rd) {
	r.skip(2)
	r.str()
	r.skip(8 + 1 + 1 + 2 + 4 + 1 + 1 + 8 + 1)
	if r.u8() != 0 {
		r.skip(24 + 8)
	}
	r.skip(4)
}

func addPlayer(r *rd) {
	r.skip(4)
	r.str()
	r.skip(12 + 3)
	r.uid()
	r.uid()
	r.skip(1 + 16)
	r.meta()
}

func addEntity(r *rd) {
	r.skip(2)
	r.jutf()
	r.skip(1 + 6 + 2)
	if r.i32() >= 0 {
		r.skip(6)
	}
}

func addMob(r *rd) {
	r.skip(2)
	r.jutf()
	r.varint()
	r.skip(6 + 3 + 6)
	r.skip(4 * r.u8())
	r.meta()
	r.cnbt()
}

func updateAttributes(r *rd) {
	r.skip(2 + 4)
	n := r.u8()
	for k := 0; k < n && r.err == nil; k++ {
		r.skip(1 + 8)
		r.skip(10 * r.u8()) // modifiers: byte, double, byte
	}
}

func blockRegion(r *rd) {
	r.skip(1 + 4 + 2 + 4 + 3)
	r.skip(int(r.u32() & 0x3fffffff))
	r.skip(6 * r.u16()) // block changes: short, int
}

func chunkBlocks(r *rd) {
	n := int(r.u32() >> 20 & 0xff)
	for k := 0; k < n && r.err == nil; k++ {
		b := r.u8()
		if b&0x40 != 0 {
			for r.u8()&0x80 != 0 && r.err == nil {
			}
		}
		if b&0x80 != 0 {
			r.skip(2)
		} else {
			r.skip(1)
		}
	}
}

func explode(r *rd) {
	if r.u8() == 0 {
		r.skip(16)
		r.skip(3 * r.i32())
	}
	r.skip(12)
}

func particles(r *rd) {
	t := r.i32()
	r.skip(1 + 28 + 4)
	r.skip(4 * particleParams[t])
	if r.u8() != 0 {
		r.skip(12)
	}
}

func containerOpen(r *rd) {
	r.skip(1)
	t := r.u8()
	r.skip(1)
	custom := r.u8()
	if t == 11 || t == 6 {
		r.skip(4)
	}
	if custom != 0 {
		r.str()
		r.str()
		r.skip(2)
	}
}

func bossEvent(r *rd) {
	r.jutf()
	switch r.u8() {
	case 0:
		r.jutf()
		r.skip(4 + 4 + 3)
	case 2:
		r.skip(4)
	case 3:
		r.jutf()
	case 4:
		r.skip(2)
	case 5:
		r.skip(1)
	}
}

func craftItem(r *rd) {
	r.skip(2 + 1)
	pairs := r.u8()
	n := r.i32()
	if pairs != 0 {
		r.skip(8 * n)
	} else {
		r.skip(4 * n)
	}
}

func textureGeometry(r *rd) {
	r.str()
	r.skip(4)
	r.skip(r.u16())
	r.skip(12 + 32)
	r.skip(40 * r.u16()) // boxes: short, 8 floats, int, short
}

// scoreboard: ScoreboardPacket::read 028a1bcc. Type 0 AddPlayerStatsData (028a2184): two bytes and all 46 records; 1
// UpdatePlayerStatsData (028a2728): two bytes, a long mask, the records whose bit is set; 2 RemovePlayerStatsData: a
// PlayerUID. Record kinds from Scoreboard::PlayerStats::readRecord 028a104c.
func scoreboard(r *rd) {
	switch r.u8() {
	case 0:
		r.skip(2)
		for k := 0; k < 46 && r.err == nil; k++ {
			record(r, k)
		}
	case 1:
		r.skip(2)
		m := r.u64()
		for k := 0; k < 46 && r.err == nil; k++ {
			if m>>k&1 != 0 {
				record(r, k)
			}
		}
	case 2:
		r.uid()
	}
}

func record(r *rd, k int) {
	switch {
	case k == 0:
		r.uid()
	case k < 3:
		r.jutf()
	case k < 6:
		r.skip(1)
	case k < 10:
		r.skip(4)
	default:
		r.varint()
	}
}

func playerTeam(r *rd) {
	r.str()
	mode := r.u8()
	if mode == 0 || mode == 2 {
		r.skip(5)
	}
	if mode == 0 || mode == 3 || mode == 4 {
		n := r.u8()
		for k := 0; k < n && r.err == nil; k++ {
			r.uid()
		}
	}
}

func mapItem(r *rd) {
	r.varint()
	r.skip(2)
	r.skip(8 * r.varint()) // decorations: eight bytes each
	if r.u8() > 0 {
		r.skip(3)
		r.skip(r.i32())
	}
}

// combat: event 1 VarInt + int, event 2 VarInt + int + utf, on both builds (Wii U read 021e82a8 branches at 021e832c, write
// 021e83d8 at 021e8460; PS3 write 0x1648dc). The 2026-11-10 note that the Wii U drops the event 2 payload missed the
// second branch.
func combat(r *rd) {
	switch r.i32() {
	case 1:
		r.varint()
		r.skip(4)
	case 2:
		r.varint()
		r.skip(4)
		r.str()
	}
}

func setBorder(r *rd) {
	switch r.u32() {
	case 0:
		r.skip(8)
	case 1:
		r.skip(16)
		r.varlong()
	case 2:
		r.skip(16)
	case 3:
		r.skip(32)
		r.varlong()
		r.varint()
		r.varint()
		r.varint()
	case 4, 5:
		r.varint()
	}
}

func mgSettings(r *rd) {
	r.varint()
	r.skip(5 + 8) // Abilities::read 0200ee34: five booleans, two floats
	r.varint()
	r.skip(5)
	r.varint()
	r.varint()
	r.skip(5 + 1)
	if r.u8() != 0 {
		r.varint()
	}
	r.skip(1)
	if r.u8() != 0 { // readMaybeFloat 021aff20
		r.skip(4)
	}
	if r.u8() != 0 {
		r.varint()
	}
	r.skip(4 + 1 + 1 + 4 + 1 + 4 + 4 + 1 + 1 + 1)
}
