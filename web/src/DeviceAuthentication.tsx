import type { DeviceMember } from './api'

export function memberName(member: DeviceMember) {
  const model = member.model.toLowerCase()
  const type = model.startsWith('audioaccessory')
    ? 'HomePod'
    : model.startsWith('appletv')
      ? 'Apple TV'
      : 'AirPlay'
  return `${member.name} · ${type}`
}

export default function DeviceAuthentication({
  member,
  disabled,
  manageable = true,
  pair,
  password,
}: {
  member: DeviceMember
  disabled: boolean
  manageable?: boolean
  pair: () => void
  password: () => void
}) {
  const name = memberName(member)
  const requirement = member.authentication?.requirement || 'unknown'
  const passwordSaved = member.authentication?.password_saved
  const saved = member.paired || passwordSaved
  const restricted = requirement === 'access_control'
  const pinRequired = requirement === 'pin' && !member.paired
  const passwordRequired = requirement === 'password' && !saved
  const status = {
    none: '无需额外配对（设备广播）',
    pin: '需要屏幕 PIN 配对',
    password: '需要 AirPlay 设备密码',
    access_control: '访问策略限制，请检查家庭或设备的 AirPlay 访问设置',
    unknown: '认证需求未知，可先尝试播放',
  }[requirement]

  return (
    <section className="device-auth" aria-label={`${name} 认证`}>
      <strong>{name}</strong>
      <p>
        {member.online ? '在线' : '离线'} ·{' '}
        {saved
          ? [member.paired && '已保存配对凭据', passwordSaved && '已保存密码，连接时验证']
              .filter(Boolean)
              .join(' · ')
          : status}
      </p>
      {saved && restricted && <p>{status}</p>}
      {!manageable ? (
        <p>此组合通过主设备管理认证。</p>
      ) : restricted ? (
        <p>现有凭据会继续保留；屏幕 PIN 或密码不能代替家庭访问授权。</p>
      ) : (
        <>
          {pinRequired && (
            <button className="pair-button" disabled={disabled} onClick={pair}>
              配对 {name}
            </button>
          )}
          {passwordRequired && (
            <button className="pair-button" disabled={disabled} onClick={password}>
              输入设备密码 {name}
            </button>
          )}
          <details>
            <summary>高级认证选项</summary>
            <p>
              已能播放时无需操作。设备广播可能不完整；仅在设备明确要求认证或连接失败时，按其提示操作。
            </p>
            {!pinRequired && requirement !== 'password' && (
              <button className="pair-button" disabled={disabled} onClick={pair}>
                {member.paired ? '重新进行屏幕 PIN 配对' : '尝试屏幕 PIN 配对'} {name}
              </button>
            )}
            {!passwordRequired && (
              <button className="pair-button" disabled={disabled} onClick={password}>
                {passwordSaved ? '更新设备密码' : '输入设备密码'} {name}
              </button>
            )}
          </details>
        </>
      )}
    </section>
  )
}
