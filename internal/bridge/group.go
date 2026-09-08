package bridge

// Fixed, user-facing reasons keep subprocess output and credentials out of the
// public snapshot. Group degradation never enters whole-output recovery.
func groupReason(state string) string {
	switch state {
	case "group_degraded_offline":
		return "Apple TV 离线，HomePod 继续播放"
	case "group_degraded_clock":
		return "共享时钟不可用，本次仅由 HomePod 播放"
	case "group_degraded_connect":
		return "Apple TV 连接失败，请检查配对和网络；HomePod 继续播放"
	case "group_degraded_auth":
		return "Apple TV 认证失败，请检查该成员的密码、配对凭据及访问设置；HomePod 继续播放"
	case "group_degraded_start":
		return "Apple TV 未确认加入，HomePod 继续播放"
	case "group_degraded_control":
		return "Apple TV 控制失败，HomePod 继续播放"
	case "group_degraded_audio", "group_degraded_member":
		return "Apple TV 音频或连接中断，HomePod 继续播放"
	case "group_degraded_timeline":
		return "组合时间线发生变化，本次仅由 HomePod 播放"
	case "group_degraded_unstable":
		return "未确认 HomePod 持续发送音频，已取消 Apple TV 加入"
	default:
		return ""
	}
}
