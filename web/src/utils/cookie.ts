// 极简 cookie 读写：只给「每个浏览器各自记一份、不需要跟服务端同步」的
// 客户端偏好用（比如网格列数）。跟账号/会话无关的 cookie 不需要带
// HttpOnly/Secure 那一套，纯前端 document.cookie 就够。

export function getCookie(name: string): string | null {
  const key = name + '='
  const parts = document.cookie.split(';')
  for (let p of parts) {
    p = p.trim()
    if (p.startsWith(key)) {
      try {
        return decodeURIComponent(p.slice(key.length))
      } catch {
        return p.slice(key.length)
      }
    }
  }
  return null
}

export function setCookie(name: string, value: string, days = 365): void {
  const expires = new Date(Date.now() + days * 86400000).toUTCString()
  document.cookie = `${name}=${encodeURIComponent(value)}; expires=${expires}; path=/; SameSite=Lax`
}
