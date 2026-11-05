//! handle 表：slot + generation，防 use-after-free 与 ABA（R-NA-03）。
//!
//! 设计取舍（为什么不是 `HashMap<u64, T>` 或 `Arc`）：
//!
//! * `Arc` 不行 —— Go 侧只允许持有一个 `uint64`，不能持有引用计数对象；
//!   而且 Arc 的释放时机由最后一个持有者决定，Go 无法判断"我 release 之后
//!   它一定已经擦除了"，这与 R-NA-04 的"释放后内存必全 0"冲突。
//! * `HashMap<u64, T>` 不行 —— 递增 id 会无限增长；随机 id 需要随机源，
//!   而随机源是"可重现构建"和"golden 测试"的敌人。
//! * `Vec<Option<Entry>>` + free list 正好：O(1) 分配/释放、内存有界、
//!   生成全由确定性算术决定。
//!
//! 核心不变量：
//!   1. 槽位 0 永远不用（handle 0 表示非法）。
//!   2. 每次释放 generation += 1；旧 handle 的 generation 不再匹配 → 返回
//!      `HM_E_HANDLE_STALE`，是**命名错误**，不是 UB。
//!   3. generation 到 `u32::MAX` 时该槽位**退役**，永不复用。
//!      没有这一条，回绕之后的 handle 会撞上一个"看起来一样但其实是新对象"
//!      的槽位 —— 这正是 ABA 问题，而且它 100% 会表现为"偶发读到别人的数据"。

use crate::status;

/// 打包：低 32 位 slot，高 32 位 generation。与 `hm_internal.h` 的
/// `hermes_handle_pack` 必须逐位一致。
#[inline]
pub const fn pack(slot: u32, generation: u32) -> u64 {
    ((generation as u64) << 32) | (slot as u64)
}

#[inline]
pub const fn unpack(handle: u64) -> (u32, u32) {
    ((handle & 0xffff_ffff) as u32, (handle >> 32) as u32)
}

struct Slot<T> {
    generation: u32,
    value: Option<T>,
}

/// 见模块文档。`T` 必须能安全地跨线程移动（所有消费者都走 `Mutex`）。
pub struct HandleTable<T> {
    slots: Vec<Slot<T>>,
    free: Vec<u32>,
    live: usize,
    /// 因为 generation 到顶而永久退役的槽位数。用于诊断（doctor / 测试）。
    retired: usize,
}

impl<T> HandleTable<T> {
    pub const fn new() -> Self {
        Self {
            slots: Vec::new(),
            free: Vec::new(),
            live: 0,
            retired: 0,
        }
    }

    /// 槽位 0 的占位。第一次插入时才创建，保持 `new()` 是 const。
    fn ensure_sentinel(&mut self) {
        if self.slots.is_empty() {
            self.slots.push(Slot {
                generation: 0,
                value: None,
            });
        }
    }

    /// 插入一个值，返回它的 handle。
    pub fn insert(&mut self, value: T) -> u64 {
        self.ensure_sentinel();

        let slot = match self.free.pop() {
            Some(s) => s,
            None => {
                self.slots.push(Slot {
                    generation: 1, // 新槽位从 1 开始；0 留给"从未使用"
                    value: None,
                });
                (self.slots.len() - 1) as u32
            }
        };

        let entry = &mut self.slots[slot as usize];
        entry.value = Some(value);
        self.live += 1;
        pack(slot, entry.generation)
    }

    /// 只读访问。generation 不匹配 / 槽位空 → `HM_E_HANDLE_STALE`。
    pub fn get(&self, handle: u64) -> Result<&T, i32> {
        let entry = self.lookup(handle)?;
        match entry.value.as_ref() {
            Some(v) => Ok(v),
            None => Err(status::HM_E_HANDLE_STALE),
        }
    }

    /// 独占访问。
    pub fn get_mut(&mut self, handle: u64) -> Result<&mut T, i32> {
        let entry = self.lookup_mut(handle)?;
        match entry.value.as_mut() {
            Some(v) => Ok(v),
            None => Err(status::HM_E_HANDLE_STALE),
        }
    }

    /// 取走值并让 handle 失效（generation += 1 或退役槽位）。
    pub fn remove(&mut self, handle: u64) -> Result<T, i32> {
        let (slot, generation) = unpack(handle);
        if slot == 0 || (slot as usize) >= self.slots.len() {
            return Err(status::HM_E_HANDLE_STALE);
        }
        {
            let entry = &self.slots[slot as usize];
            if entry.generation != generation {
                return Err(status::HM_E_HANDLE_STALE);
            }
        }

        let entry = &mut self.slots[slot as usize];
        let value = match entry.value.take() {
            Some(v) => v,
            None => return Err(status::HM_E_HANDLE_STALE),
        };
        self.live -= 1;

        if entry.generation == u32::MAX {
            // 退役：不回 free list，`alloc` 永远不会再选中它。
            self.retired += 1;
        } else {
            entry.generation += 1;
            self.free.push(slot);
        }
        Ok(value)
    }

    fn lookup(&self, handle: u64) -> Result<&Slot<T>, i32> {
        let (slot, generation) = unpack(handle);
        if slot == 0 || (slot as usize) >= self.slots.len() {
            return Err(status::HM_E_HANDLE_STALE);
        }
        let entry = &self.slots[slot as usize];
        if entry.generation != generation {
            return Err(status::HM_E_HANDLE_STALE);
        }
        Ok(entry)
    }

    fn lookup_mut(&mut self, handle: u64) -> Result<&mut Slot<T>, i32> {
        let (slot, generation) = unpack(handle);
        if slot == 0 || (slot as usize) >= self.slots.len() {
            return Err(status::HM_E_HANDLE_STALE);
        }
        let entry = &mut self.slots[slot as usize];
        if entry.generation != generation {
            return Err(status::HM_E_HANDLE_STALE);
        }
        Ok(entry)
    }

    pub fn live(&self) -> usize {
        self.live
    }

    pub fn retired(&self) -> usize {
        self.retired
    }

    /// 运行时关闭时清空一切（保证 shutdown 之后没有残留敏感数据）。
    pub fn clear(&mut self) {
        self.slots.clear();
        self.free.clear();
        self.live = 0;
        self.retired = 0;
    }

    // ---- 测试专用：把 generation 推到指定值，用来复现 ABA 边界 ----
    #[cfg(test)]
    pub fn force_generation(&mut self, slot: u32, generation: u32) {
        self.slots[slot as usize].generation = generation;
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn insert_get_remove_roundtrip() {
        let mut t: HandleTable<u32> = HandleTable::new();
        let h = t.insert(7);
        assert_ne!(h, 0);
        assert_eq!(*t.get(h).unwrap(), 7);
        assert_eq!(t.live(), 1);
        assert_eq!(t.remove(h).unwrap(), 7);
        assert_eq!(t.live(), 0);
    }

    #[test]
    fn stale_handle_after_free_is_named_error() {
        let mut t: HandleTable<u32> = HandleTable::new();
        let h = t.insert(1);
        assert!(t.remove(h).is_ok());
        // R-NA-03：释放后使用旧 handle 必须是命名错误，不是崩溃、不是 UB。
        assert_eq!(t.get(h).unwrap_err(), status::HM_E_HANDLE_STALE);
        assert_eq!(t.remove(h).unwrap_err(), status::HM_E_HANDLE_STALE);
    }

    #[test]
    fn recycled_slot_gets_new_generation() {
        let mut t: HandleTable<u32> = HandleTable::new();
        let a = t.insert(1);
        let (slot_a, gen_a) = unpack(a);
        assert!(t.remove(a).is_ok());
        let b = t.insert(2);
        let (slot_b, gen_b) = unpack(b);
        // 槽位复用（省内存），但 generation 必须变（防 ABA）。
        assert_eq!(slot_a, slot_b);
        assert_ne!(gen_a, gen_b);
        assert_eq!(t.get(a).unwrap_err(), status::HM_E_HANDLE_STALE);
        assert_eq!(*t.get(b).unwrap(), 2);
    }

    #[test]
    fn generation_overflow_retires_the_slot() {
        let mut t: HandleTable<u32> = HandleTable::new();
        let a = t.insert(1);
        let (slot, _) = unpack(a);

        // 手工把 generation 推到 u32::MAX - 1：下一次 remove 会把它推到
        // u32::MAX，槽位必须退役。
        t.force_generation(slot, u32::MAX - 1);
        let h = pack(slot, u32::MAX - 1);
        assert!(t.remove(h).is_ok());
        assert_eq!(t.retired(), 1);

        // 再来一次分配：必须**跳过**这个退役槽位。
        let b = t.insert(2);
        let (slot_b, _) = unpack(b);
        assert_ne!(slot_b, slot, "退化的槽位必须被跳过，否则 ABA 回来了");

        // 用退役槽位 + generation=u32::MAX 拼出来的 handle 必须无效。
        assert_eq!(
            t.get(pack(slot, u32::MAX)).unwrap_err(),
            status::HM_E_HANDLE_STALE
        );
    }

    #[test]
    fn zero_handle_is_never_valid() {
        let t: HandleTable<u32> = HandleTable::new();
        assert_eq!(t.get(0).unwrap_err(), status::HM_E_HANDLE_STALE);
    }

    #[test]
    fn out_of_range_slot_is_stale_not_panic() {
        let mut t: HandleTable<u32> = HandleTable::new();
        let _ = t.insert(1);
        // 高 32 位随意、低 32 位超大：必须是错误而不是越界读。
        assert_eq!(
            t.get(pack(9999, 1)).unwrap_err(),
            status::HM_E_HANDLE_STALE
        );
        assert_eq!(
            t.remove(pack(9999, 1)).unwrap_err(),
            status::HM_E_HANDLE_STALE
        );
    }
}
