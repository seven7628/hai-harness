// 内置 provider 的注册入口：整个应用只 import 一次（main.tsx）。
// 注意：这里靠**副作用**注册，不要改成「导出数组再遍历」——那会让 tree-shaking 把没人引用的 provider 删掉。
import './gitSurface'
import './fileSurface'
import './webSurface'
import './activitySurface'
