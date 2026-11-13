# hermesConfig.cmake —— 让 CMake 项目用几行接入 Hermes。
#
#   find_package(hermes REQUIRED)
#   target_link_libraries(app PRIVATE hermes::runtime)
#   hermes_add_unit(app units/label.hermes)
#
# 注意：**这份文件在本机没有实测过**（开发机没装 cmake）。
# 逻辑按 hermes 的 CLI 契约写；第一次真实构建如果出问题，修这里，
# 不要在用户项目里堆 workaround。

include(CMakeParseArguments)

# 显式给了 HERMES_ROOT 就用它 —— **不要**先 find_program：
# PATH 里可能装着另一个版本的 hermes，静默选它会让"我要用这份构建"变成运气。
set(HERMES_EXECUTABLE "")
if(HERMES_ROOT AND EXISTS "${HERMES_ROOT}/bin/hermes")
  set(HERMES_EXECUTABLE "${HERMES_ROOT}/bin/hermes")
elseif(HERMES_ROOT AND EXISTS "${HERMES_ROOT}/build/hermes")
  set(HERMES_EXECUTABLE "${HERMES_ROOT}/build/hermes")
endif()
if(NOT HERMES_EXECUTABLE)
  find_program(HERMES_EXECUTABLE NAMES hermes HINTS "$ENV{HERMES_HOME}/bin")
endif()
if(NOT HERMES_EXECUTABLE)
  message(FATAL_ERROR "找不到 hermes；设置 HERMES_ROOT 或把它的 bin 加进 PATH")
endif()

get_filename_component(_hermes_bin "${HERMES_EXECUTABLE}" DIRECTORY)
get_filename_component(_hermes_root "${_hermes_bin}" DIRECTORY)

# 三种布局的差异收敛到这里：安装树有 <root>/lib 与 <root>/include；
# 构建树里可执行文件在 <root>/build，头文件在 <root>/include，静态库在 <root>/build。
if(EXISTS "${_hermes_root}/lib/libhermesrt.a")
  set(_hermes_lib "${_hermes_root}/lib/libhermesrt.a")
  set(_hermes_inc "${_hermes_root}/include")
else()
  set(_hermes_lib "${_hermes_bin}/libhermesrt.a")
  set(_hermes_inc "${_hermes_root}/include")
endif()

# **没有** hermes::runtime 这个导入目标。原因：`hermes cc` 会为每个 unit
# 生成一份 C shim（把 per-unit 符号接到 hermes_rt_run、失败时发布错误文本），
# 并把 CoreImpl 写出的 hermes_core_impl.c（hermes_buf_free /
# hermes_last_error / 共享错误槽的强定义）一起编译链接，所以"链一个静态库"
# 是不完整的 —— 手写 clang 会报 hermes_<unit>_<wf> / hermes_last_error
# 找不到。正确的用法是让 hermes cc 做最后的链接，见下面的 hermes_link()。

# hermes_add_unit(<target> <unit.hermes>...)
#
# 只做"生成"这一半：编出 .hbc、生成 C 头、把目录加进 include 路径。
# 链接那一半由 hermes_link() 负责。
function(hermes_add_unit target)
  set(units ${ARGN})
  if(NOT units)
    message(FATAL_ERROR "hermes_add_unit(${target}) 需要至少一个 .hermes 文件")
  endif()

  foreach(unit IN LISTS units)
    # 用绝对路径：自定义命令的工作目录是 build 树，相对路径会指向不存在的位置。
    get_filename_component(unit_abs "${unit}" ABSOLUTE BASE_DIR "${CMAKE_CURRENT_SOURCE_DIR}")
    get_filename_component(unit_name "${unit}" NAME_WE)
    set(out_dir "${CMAKE_CURRENT_BINARY_DIR}/hermes")
    set(hbc "${out_dir}/${unit_name}.hbc")
    set(hdr "${out_dir}/${unit_name}.h")

    add_custom_command(
      OUTPUT "${hbc}" "${hdr}"
      COMMAND ${CMAKE_COMMAND} -E make_directory "${out_dir}"
      COMMAND "${HERMES_EXECUTABLE}" build "${unit_abs}" -o "${hbc}"
      COMMAND "${HERMES_EXECUTABLE}" header "${hbc}" --c -o "${hdr}"
      DEPENDS "${unit_abs}"
      COMMENT "hermes: ${unit_name}"
      VERBATIM)

    add_custom_target(hermes_${target}_${unit_name} DEPENDS "${hbc}" "${hdr}")
    add_dependencies(${target} hermes_${target}_${unit_name})

    # 用目录级 include_directories 而不是 target_include_directories：
    # hermes_add_unit 的 target 可能是个 custom target（只负责生成），
    # 而 custom target 不接受 target_include_directories。
    include_directories("${out_dir}" "${_hermes_inc}")
    # **不要**把 .hbc 加进 target_sources：CMake 不认识这种源文件类型，
    # 会报 "CMake does not know the flags to enable it"。
    # 依赖关系已经由 add_dependencies 建立，链接产物由 hermes cc 负责。
  endforeach()
endfunction()


# hermes_link(<output> <source.c> <unit.hbc>...)
#
# 用 hermes cc 完成编译 + 链接：它会把每个 unit 的 C shim 生成出来并一起编译。
# 这不是"多余的包装"——shim 是每个 unit 特有的代码，CMake 生成不出来。
function(hermes_link output source)
  set(units ${ARGN})
  set(deps "")
  set(unit_args "")
  foreach(unit IN LISTS units)
    get_filename_component(unit_abs "${unit}" ABSOLUTE BASE_DIR "${CMAKE_CURRENT_SOURCE_DIR}")
    get_filename_component(unit_name "${unit}" NAME_WE)
    set(hbc "${CMAKE_CURRENT_BINARY_DIR}/hermes/${unit_name}.hbc")
    list(APPEND deps "${hbc}")
    list(APPEND unit_args --unit "${hbc}")
  endforeach()

  add_custom_command(
    OUTPUT "${output}"
    COMMAND "${HERMES_EXECUTABLE}" cc "${source}" ${unit_args} -o "${output}"
    DEPENDS "${source}" ${deps}
    COMMENT "hermes cc: ${output}"
    VERBATIM)
  # 目标名不能带斜杠：用输出文件名。
  get_filename_component(_name "${output}" NAME)
  add_custom_target(${_name} ALL DEPENDS "${output}")
endfunction()
