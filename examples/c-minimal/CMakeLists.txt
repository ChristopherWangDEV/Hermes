# CMake 集成示例。
#
#   cmake -S . -B build -DHERMES_ROOT=$HOME/.local/hermes
#   cmake --build build
#   ./build/app
#
# 注意：链接走 `hermes cc`（见 hermes_link 的注释）—— 每个 unit 需要一份
# 生成的 C shim，CMake 自己链静态库是不完整的。
cmake_minimum_required(VERSION 3.16)
project(hermes_c_minimal C)

if(EXISTS "${CMAKE_CURRENT_SOURCE_DIR}/../../cmake/hermesConfig.cmake")
  set(hermes_DIR "${CMAKE_CURRENT_SOURCE_DIR}/../../cmake")
endif()
find_package(hermes CONFIG REQUIRED)

# 1) 编 unit + 生成头（供编辑器与 include 使用）
add_custom_target(headers ALL)
hermes_add_unit(headers units/label.hermes)

# 2) 编译 + 链接：走 hermes cc
hermes_link("${CMAKE_CURRENT_BINARY_DIR}/app" "${CMAKE_CURRENT_SOURCE_DIR}/main.c"
            units/label.hermes)
