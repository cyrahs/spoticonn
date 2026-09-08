package bridge

import "strings"

// Fixed, user-facing reasons keep subprocess output and credentials out of the
// public snapshot. Group degradation never enters whole-output recovery.
func groupReason(state string) string {
	if reason, retry := strings.CutPrefix(state, "group_retrying_"); retry {
		if text := groupReason("group_degraded_" + reason); text != "" {
			return text + "；正在重试 Apple TV"
		}
		return ""
	}
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
	case "group_degraded_audio":
		return "Apple TV 音频写入失败，HomePod 继续播放"
	case "group_degraded_write_timeout":
		return "Apple TV 音频写入超时，HomePod 继续播放"
	case "group_degraded_pipe_closed":
		return "Apple TV 音频管道关闭，HomePod 继续播放"
	case "group_degraded_short_write":
		return "Apple TV 音频写入不完整，HomePod 继续播放"
	case "group_degraded_backpressure":
		return "Apple TV 音频队列已满，HomePod 继续播放"
	case "group_degraded_disconnected", "group_degraded_member":
		return "Apple TV 连接中断，HomePod 继续播放"
	case "group_degraded_error":
		return "Apple TV 引擎报告错误，HomePod 继续播放"
	case "group_degraded_clock_stalled":
		return "Apple TV 时钟停滞，HomePod 继续播放"
	case "group_degraded_timeline_changed":
		return "Apple TV 时间线发生变化，已停止该成员；HomePod 继续播放"
	case "group_degraded_homepod_timeline_changed":
		return "HomePod 时间线发生变化，已停止 Apple TV 加入；HomePod 继续播放"
	case "group_degraded_homepod_start":
		return "HomePod 未确认开始，已取消 Apple TV 加入"
	case "group_degraded_homepod_error":
		return "HomePod 引擎报告错误，已取消 Apple TV 加入"
	case "group_degraded_homepod_disconnected":
		return "HomePod 连接中断，已取消 Apple TV 加入"
	case "group_degraded_homepod_clock_stalled":
		return "HomePod 时钟停滞，已取消 Apple TV 加入"
	case "group_degraded_timeline":
		return "组合时间线发生变化，本次仅由 HomePod 播放"
	case "group_degraded_unstable", "group_degraded_homepod_unstable":
		return "未确认 HomePod 持续发送音频，已取消 Apple TV 加入"
	default:
		return ""
	}
}
