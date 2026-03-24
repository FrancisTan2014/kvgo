package raft

import "kvgo/raftpb"

func IsLocalMsg(msgType raftpb.MessageType) bool {
	return msgType == raftpb.MessageType_MsgBeat ||
		msgType == raftpb.MessageType_MsgHup ||
		msgType == raftpb.MessageType_MsgProp
}
